// -------------------------------------------------------------------------------
// Object Manager - Ranged Fetch for Compressed Reads
//
// Author: Alex Freidah
//
// The proxy-side half of compression.RangeFetcher: it turns a request for a
// range of an object's compressed bytes into one ranged backend GET, mapping
// through the encryption layer when the stored bytes are an envelope, and
// charges each fetch against the backend's meters.
// -------------------------------------------------------------------------------

package object

import (
	"context"
	"fmt"
	"io"
	"sync"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/readpath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// storedRangeFetcher fetches ranges of the compressed stream backing one copy
// of one object on one backend.
//
// That compressed stream is exactly the encryptor's plaintext domain -
// compress-then-encrypt is what makes PlaintextSize the post-compression size -
// so encryption.CiphertextRange translates a compressed-domain range into a
// backend range with no new offset math.
//
// attrs records the object metadata off the first response. A compressed read
// issues no whole-object GET, so this is the only place the content type, ETag
// and user metadata arrive; capturing them from a fetch the read already makes
// avoids paying for a HEAD to learn them. Guarded because the seekable reader
// may fetch frames concurrently.
type storedRangeFetcher struct {
	rt     RangeFetchRuntime
	be     s3be.ObjectBackend
	enc    *encryption.Encryptor
	loc    *core.ObjectLocation
	key    string
	beName string

	mu    sync.Mutex
	attrs *s3be.GetObjectResult
}

// newStoredRangeFetcher builds a fetcher for one copy. enc may be nil only when
// the copy is not encrypted; loc is required.
func newStoredRangeFetcher(rt RangeFetchRuntime, be s3be.ObjectBackend, enc *encryption.Encryptor, loc *core.ObjectLocation, key, beName string) *storedRangeFetcher {
	return &storedRangeFetcher{rt: rt, be: be, enc: enc, loc: loc, key: key, beName: beName}
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
	if !f.rt.Usage().WithinLimits(f.beName, 1, 0, 0) {
		return nil, fmt.Errorf("backend %s: %w", f.beName, readpath.ErrUsageLimitSkip)
	}

	r, cancel, err := f.rt.GetWithTimeout(ctx, f.be, f.key, header)
	if err != nil {
		f.rt.Acct().APICall(f.beName)
		return nil, fmt.Errorf("backend %s: fetch %s: %w", f.beName, header, err)
	}
	defer cancel()
	defer func() { _ = r.Body.Close() }()

	if !f.rt.Usage().WithinLimits(f.beName, 1, r.Size, 0) {
		f.rt.Acct().APICall(f.beName)
		return nil, fmt.Errorf("backend %s egress: %w", f.beName, readpath.ErrUsageLimitSkip)
	}
	f.rt.Acct().Egress(f.beName, r.Size)
	// Counted here rather than per client request: a compressed read makes one
	// fetch per frame it touches, and read amplification is exactly the sum of
	// those against what the client was served.
	telemetry.CompressionFetchedBytesTotal.Add(float64(r.Size))
	f.recordAttrs(r)

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

// recordAttrs keeps the object metadata off the first response that carries it.
// Every fetch returns the same values, so the first one wins and the rest are
// dropped rather than churning the field under the lock.
func (f *storedRangeFetcher) recordAttrs(r *s3be.GetObjectResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.attrs != nil {
		return
	}
	f.attrs = &s3be.GetObjectResult{
		ContentType:  r.ContentType,
		ETag:         r.ETag,
		LastModified: r.LastModified,
		Metadata:     r.Metadata,
	}
}

// objectAttrs returns the metadata captured from the backend, or nil when no
// fetch has completed yet.
func (f *storedRangeFetcher) objectAttrs() *s3be.GetObjectResult {
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
		return fmt.Sprintf("bytes=%d-%d", start, end), nil, nil
	}
	if f.enc == nil {
		return "", nil, fmt.Errorf("backend %s: %w", f.beName, core.ErrServiceUnavailable)
	}
	rng, err := encryption.CiphertextRange(start, end, f.enc.ChunkSize())
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
