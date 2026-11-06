// -------------------------------------------------------------------------------
// Object Manager - Compressed Reads
//
// Author: Alex Freidah
//
// A compressed object is served by decoding it, not by handing the stored bytes
// back. The seek table is read first, so a damaged copy fails over before any
// header is sent. A range then pulls frames through a RangeFetcher as the client
// reads, which costs the frames it covers rather than the object, the whole
// reason the format is chunked. A whole read of a large object streams the stored
// bytes in one GET instead of paying a backend round trip per frame.
// -------------------------------------------------------------------------------

package object

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/internalkey"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/readpath"
	"github.com/afreidah/s3-orchestrator/internal/proxy/reconcile"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/byterange"
	"github.com/afreidah/s3-orchestrator/internal/util/ioutilx"
)

// streamMinSize is the object size above which a whole read streams the stored
// bytes. A stream allocates a decoder per call, which costs a small object about
// 40% more than decoding its frames, and the two are level at 1 MiB.
const streamMinSize = 1 << 20

// -------------------------------------------------------------------------
// ROW INSPECTION
// -------------------------------------------------------------------------

// readLocation returns the row a GET or HEAD reads with: the database row when
// there is one, or, during a database outage, a row built from the newest copy
// on the backend and what that copy's bytes say about how they are stored.
//
// It classifies that copy's head and tail the way import does. An encrypted
// copy returns 503, because decrypting it needs the key only the database
// holds.
func (o *Manager) readLocation(ctx context.Context, be s3be.ObjectBackend, beName, key string, loc *core.ObjectLocation) (*core.ObjectLocation, error) {
	if loc != nil {
		return loc, nil
	}
	newest, size, err := o.newestCopy(ctx, be, beName, key)
	if err != nil {
		return nil, err
	}
	discovered, err := reconcile.DiscoverBytes(ctx, chargedReader{rt: o.d.Core, name: beName, be: be}, o.d.Codec, newest, size)
	if err != nil {
		return nil, err
	}
	_, form := core.ClassifyImport(discovered, nil)
	if form.Form().Has(core.StoredEncrypted) {
		return nil, core.ErrServiceUnavailable
	}
	return core.ObjectFromStoredForm(key, beName, newest, size, form, nil), nil
}

// newestCopy returns the path and size of the object's newest copy on one
// backend.
//
// It lists the backend starting at the object key. S3 lists in byte order and
// "!" sorts before letters, digits and ".", so the bare key "bucket/photo.jpg"
// and its copies "bucket/photo.jpg!3f9a0c..." come first, and the listing stops
// at the first key past them. The newest match wins. With no match it returns
// the bare key, so the read that follows 404s as it would anyway.
func (o *Manager) newestCopy(ctx context.Context, be s3be.ObjectBackend, beName, key string) (string, int64, error) {
	copies := internalkey.CopyPrefix(key)
	newest, size, newestAt, found := key, int64(0), time.Time{}, false
	err := be.ListObjects(ctx, key, func(page []s3be.ListedObject) error {
		for i := range page {
			listed := page[i].Key
			if listed > copies && !strings.HasPrefix(listed, copies) {
				return s3be.ErrStopListing
			}
			if !internalkey.IsCopyOf(key, listed) {
				continue
			}
			if !found || page[i].LastModified.After(newestAt) {
				newest, size, newestAt, found = listed, page[i].SizeBytes, page[i].LastModified, true
			}
		}
		return nil
	})
	o.d.Core.Acct().APICall(ctx, s3op.ListObjectsV2, beName)
	return newest, size, err
}

// resolveLastModified prefers the stored write time, which is the same on every
// copy, so failover does not move Last-Modified under If-Modified-Since and
// If-Range. It falls back to the backend's value when there is no row or the
// stored time is zero, so every response still carries a timestamp.
func resolveLastModified(backendValue time.Time, loc *core.ObjectLocation) time.Time {
	if loc != nil && !loc.CreatedAt.IsZero() {
		return loc.CreatedAt
	}
	return backendValue
}

// -------------------------------------------------------------------------
// COMPRESSED READ
// -------------------------------------------------------------------------

// compressedGetAttempt is the per-backend GET callback for a compressed copy. The
// codec pulls frames through storedRangeFetcher as the client reads, so a ranged
// read fetches only the frames covering the range, and a whole read above
// streamMinSize swaps those frames for one stream.
//
// A frame read does not check the envelope signature at byte 0, because the first
// bytes fetched are the seek table at the end. A row that disagrees with its
// bytes still fails: a copy it calls encrypted fails to decrypt, and one it calls
// plaintext fails to decode.
func (o *Manager) compressedGetAttempt(ctx context.Context, key, rangeHeader, beName string, backend s3be.ObjectBackend, loc *core.ObjectLocation) (readpath.ProbeResult[*s3be.GetObjectResult], error) {
	var fail readpath.ProbeResult[*s3be.GetObjectResult]

	if !o.d.Core.Acct().Allow(beName, s3op.GetObject, 0, 0) {
		return fail, fmt.Errorf("backend %s: %w", beName, readpath.ErrUsageLimitSkip)
	}
	if o.d.Codec == nil {
		return fail, fmt.Errorf("backend %s: %w: object is compressed but no codec is configured",
			beName, core.ErrServiceUnavailable)
	}
	if err := core.ValidateEncryptionMetadata(loc); err != nil {
		telemetry.EncryptionFlagMismatchTotal.WithLabelValues("get").Inc()
		return fail, fmt.Errorf("backend %s: %w", beName, err)
	}

	fetcher := newStoredRangeFetcher(o.d.Core, backend, o.d.Encryptor, loc, core.StoragePath(key, loc.StorageKey), beName)
	reader, err := o.d.Codec.DecompressRanged(ctx, fetcher, fetcher.compressedSize())
	if err != nil {
		telemetry.CompressionErrorsTotal.WithLabelValues(telemetry.CompressionOpDecode).Inc()
		return fail, fmt.Errorf("backend %s: %w", beName, err)
	}

	r, err := o.compressedResult(reader, fetcher, loc, rangeHeader)
	if err != nil {
		telemetry.CompressionErrorsTotal.WithLabelValues(telemetry.CompressionOpDecode).Inc()
		_ = reader.Close()
		return fail, fmt.Errorf("backend %s: %w", beName, err)
	}
	if r.ContentRange == "" && r.Size > streamMinSize {
		stream, err := fetcher.stream(ctx, o.d.Codec)
		_ = reader.Close()
		if err != nil {
			return fail, err
		}
		r.Body = stream
	}
	// The response size is what the client was promised, which is the
	// denominator read amplification is measured against.
	telemetry.CompressionServedBytesTotal.Add(float64(r.Size))

	o.maybeWrapIntegrityReader(ctx, r, loc, key, beName, r.ContentRange != "")
	return readpath.ProbeResult[*s3be.GetObjectResult]{
		Value:   r,
		Size:    r.Size,
		Cleanup: func() { _ = r.Body.Close() },
	}, nil
}

// compressedResult assembles the client-facing response over a decoded reader,
// applying the client's range in logical coordinates. The headers come from the
// fetcher, which already received a response while reading the seek table.
func (o *Manager) compressedResult(reader io.ReadSeekCloser, fetcher *storedRangeFetcher, loc *core.ObjectLocation, rangeHeader string) (*s3be.GetObjectResult, error) {
	size := loc.ObjectSize()
	r := &s3be.GetObjectResult{Body: reader, Size: size}
	if attrs := fetcher.objectAttrs(); attrs != nil {
		r.ObjectAttrs = *attrs
	}
	r.LastModified = resolveLastModified(r.LastModified, loc)
	if rangeHeader == "" {
		return r, nil
	}

	// An unsatisfiable range is served as the whole object. RFC 9110 lets a
	// server ignore a Range it cannot act on, and that is what the uncompressed
	// path does with one it cannot translate.
	start, end, ok := byterange.Parse(rangeHeader, size)
	if !ok {
		return r, nil
	}
	if _, err := reader.Seek(start, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek to %d: %w", start, err)
	}
	r.Size = end - start + 1
	r.Body = ioutilx.ReadCloser(io.LimitReader(reader, r.Size), reader)
	r.ContentRange = byterange.ContentRange(start, end, size)
	return r, nil
}
