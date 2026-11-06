// -------------------------------------------------------------------------------
// Object Manager - Ranged Fetch for Compressed Reads
//
// Author: Alex Freidah
//
// The proxy-side half of compression.RangeFetcher: it turns a request for a
// range of an object's compressed bytes into one ranged backend GET, mapping
// through the encryption layer when the stored bytes are an envelope, and
// charges each fetch against the backend's meters. It also opens the whole copy
// as one stream, for a read that wants all of it.
// -------------------------------------------------------------------------------

package object

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/readpath"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/storedform"
	"github.com/afreidah/s3-orchestrator/internal/util/byterange"
	"github.com/afreidah/s3-orchestrator/internal/util/ioutilx"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// storedRangeFetcher fetches ranges of the compressed stream backing one copy
// of one object on one backend. The compressed stream is the encryptor's
// plaintext domain, so encryption.CiphertextRange translates a compressed-domain
// range into a backend range directly.
//
// attrs holds the object metadata from the first response, which is the seek
// table fetch. mu guards it because the seekable reader may fetch frames
// concurrently.
type storedRangeFetcher struct {
	rt         RangeFetchRuntime
	be         s3be.ObjectBackend
	enc        *encryption.Encryptor
	loc        *core.ObjectLocation
	storageKey string
	beName     string

	mu    sync.Mutex
	attrs *s3be.ObjectAttrs
}

// chargedReader reads ranges of one backend's objects under the runtime's
// backend timeout, charging each read like any other GET.
type chargedReader struct {
	rt   RangeFetchRuntime
	name string
	be   s3be.ObjectBackend
}

// ReadRange implements backend.RangeReader. A failed read is charged by the
// runtime and a successful one with the bytes the backend sent.
func (c chargedReader) ReadRange(ctx context.Context, key string, start, end int64) ([]byte, error) {
	r, cancel, err := c.rt.Get(ctx, c.name, c.be, key, byterange.Header(start, end))
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer func() { _ = r.Body.Close() }()
	c.rt.Acct().Egress(ctx, s3op.GetObject, c.name, r.WireBytes())
	return s3be.ReadSpan(r.Body, start, end)
}

// newStoredRangeFetcher builds a fetcher for one copy, which unwraps the copy's
// key once however many fetches it serves. enc may be nil only when the copy is
// not encrypted; loc is required. storageKey is the path the copy occupies on
// its backend, which every fetch is issued against.
func newStoredRangeFetcher(rt RangeFetchRuntime, be s3be.ObjectBackend, enc *encryption.Encryptor, loc *core.ObjectLocation, storageKey, beName string) *storedRangeFetcher {
	if enc != nil {
		enc = enc.ForOneObject()
	}
	return &storedRangeFetcher{rt: rt, be: be, enc: enc, loc: loc, storageKey: storageKey, beName: beName}
}

// compressedSize reports the size of the compressed stream this fetcher serves.
// For an encrypted copy the stored bytes are its ciphertext, so the size comes
// from PlaintextSize rather than from what the backend holds.
func (f *storedRangeFetcher) compressedSize() int64 {
	if f.loc.Encrypted {
		return f.loc.PlaintextSize
	}
	return f.loc.SizeBytes
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// FetchRange implements compression.RangeFetcher.
//
// Each fetch is charged its own API call and egress: a compressed read makes
// one backend call per frame it touches, so charging once per client GET would
// under-report all but the first.
func (f *storedRangeFetcher) FetchRange(ctx context.Context, start, end int64) ([]byte, error) {
	header, rng, err := f.backendRangeFor(start, end)
	if err != nil {
		return nil, err
	}
	r, cancel, err := f.get(ctx, header)
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer func() { _ = r.Body.Close() }()

	body, err := f.compressedBody(ctx, r, rng)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, end-start+1)
	if _, err := io.ReadFull(body, buf); err != nil {
		return nil, fmt.Errorf("backend %s: read %s: %w", f.beName, header, err)
	}
	return buf, nil
}

// stream opens the whole copy in one GET and undoes its stored form, returning
// the client's bytes. The body owns the GET's timeout cancel through Close, as an
// uncompressed read's does.
func (f *storedRangeFetcher) stream(ctx context.Context, codec storedform.Decompressor) (io.ReadCloser, error) {
	r, cancel, err := f.get(ctx, "")
	if err != nil {
		return nil, err
	}
	body := ioutilx.WithCancel(r.Body, cancel)
	decoded, closeDecoded, err := storedform.Open(ctx, f.enc, codec, body, &f.loc.StoredForm)
	if err != nil {
		_ = body.Close()
		return nil, fmt.Errorf("backend %s: %w", f.beName, err)
	}
	return ioutilx.WithCancel(ioutilx.ReadCloser(decoded, body), closeDecoded), nil
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// get issues one metered GET and charges what the backend sent. The fetched
// bytes are counted per GET rather than per client request, since read
// amplification is the sum of these against what the client was served.
func (f *storedRangeFetcher) get(ctx context.Context, header string) (*s3be.GetObjectResult, context.CancelFunc, error) {
	r, cancel, err := meteredGet(ctx, f.rt, f.be, f.beName, f.storageKey, header)
	if errors.Is(err, readpath.ErrUsageLimitSkip) {
		return nil, nil, err
	}
	if err != nil {
		return nil, nil, fmt.Errorf("backend %s: fetch %q: %w", f.beName, header, err)
	}
	wire := r.WireBytes()
	f.rt.Acct().Egress(ctx, s3op.GetObject, f.beName, wire)
	telemetry.CompressionFetchedBytesTotal.Add(float64(wire))
	f.recordAttrs(r)
	return r, cancel, nil
}

// recordAttrs keeps the object metadata off the first response that carries it.
// Every fetch returns the same values, so the first one wins and the rest are
// dropped rather than churning the field under the lock.
func (f *storedRangeFetcher) recordAttrs(r *s3be.GetObjectResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.attrs != nil {
		return
	}
	attrs := r.ObjectAttrs
	f.attrs = &attrs
}

// objectAttrs returns the metadata captured from the backend, or nil when no
// fetch has completed yet.
func (f *storedRangeFetcher) objectAttrs() *s3be.ObjectAttrs {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attrs
}

// backendRangeFor translates a compressed-domain range into the Range header to
// send the backend. Unencrypted stored bytes are already in that domain and
// pass through; the RangeResult returned for an encrypted copy is what slices
// the requested bytes back out after decryption.
func (f *storedRangeFetcher) backendRangeFor(start, end int64) (string, *encryption.RangeResult, error) {
	if start < 0 || end < start {
		return "", nil, fmt.Errorf("%w: %d-%d", core.ErrInvalidRange, start, end)
	}
	if !f.loc.Encrypted {
		return byterange.Header(start, end), nil, nil
	}
	if f.enc == nil {
		return "", nil, fmt.Errorf("backend %s: %w", f.beName, core.ErrServiceUnavailable)
	}
	rng, err := encryption.CiphertextRange(start, end, f.loc.PlaintextSize, f.loc.SizeBytes)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %w", core.ErrInvalidRange, err)
	}
	return rng.BackendRange, rng, nil
}

// compressedBody unwraps a backend response into the compressed bytes asked
// for, decrypting when the copy is an envelope.
func (f *storedRangeFetcher) compressedBody(ctx context.Context, r *s3be.GetObjectResult, rng *encryption.RangeResult) (io.Reader, error) {
	if rng == nil {
		return r.Body, nil
	}
	plain, _, err := f.enc.DecryptStored(ctx, r.Body, f.loc.EncryptionKey, f.loc.KeyID, f.loc.PlaintextSize, rng)
	if err != nil {
		return nil, fmt.Errorf("backend %s: decrypt_range: %w", f.beName, err)
	}
	return plain, nil
}
