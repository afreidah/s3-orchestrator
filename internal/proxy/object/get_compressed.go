// -------------------------------------------------------------------------------
// Object Manager - Compressed Reads
//
// Author: Alex Freidah
//
// A compressed object is served by decoding it, not by handing the stored bytes
// back. The stored form is chunked zstd, so the read is driven by the codec:
// there is no whole-object GET, and frames are pulled through a RangeFetcher as
// the client reads. A range therefore costs the frames it covers rather than
// the object, which is the whole reason the format is chunked.
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
	"github.com/afreidah/s3-orchestrator/internal/util/ioutilx"
)

// -------------------------------------------------------------------------
// ROW INSPECTION
// -------------------------------------------------------------------------

// isCompressed reports whether a copy's stored bytes were encoded. An empty
// algorithm is the store's own way of saying the bytes are verbatim, so this
// never consults a separate flag that could disagree with it.
func isCompressed(loc *core.ObjectLocation) bool {
	return loc != nil && loc.CompressionAlgorithm != ""
}

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
	discovered, err := reconcile.DiscoverBytes(ctx, be, o.codec, newest, size)
	if err != nil {
		return nil, err
	}
	_, form := core.ClassifyImport(discovered, nil)
	if form != nil && form.Encrypted {
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
	copies := key + internalkey.WriteSeparator
	newest, size, newestAt, found := key, int64(0), time.Time{}, false
	err := be.ListObjects(ctx, key, func(page []s3be.ListedObject) error {
		for i := range page {
			listed := page[i].Key
			if listed > copies && !strings.HasPrefix(listed, copies) {
				return s3be.ErrStopListing
			}
			if listed != key && !isPerWritePath(key, listed) {
				continue
			}
			if !found || page[i].LastModified.After(newestAt) {
				newest, size, newestAt, found = listed, page[i].SizeBytes, page[i].LastModified, true
			}
		}
		return nil
	})
	o.core.Acct().APICall(s3op.ListObjectsV2, beName)
	return newest, size, err
}

// writeIDLength is the number of characters after the "!" in a copy's path:
// a random 16-byte id written as 32 lowercase hex characters.
const writeIDLength = 32

// isPerWritePath reports whether a listed key is a stored copy of key rather
// than a different object sharing its prefix, such as a client's
// "photo.jpg!backup". A copy's path is the key, "!", then exactly 32 lowercase
// hex characters.
func isPerWritePath(key, listed string) bool {
	id, ok := strings.CutPrefix(listed, key+internalkey.WriteSeparator)
	if !ok || len(id) != writeIDLength {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
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

// logicalSize reports the size the client wrote. For a compressed copy that is
// the only place it survives, since SizeBytes counts the stored bytes and
// PlaintextSize counts the compressed stream inside them.
func logicalSize(loc *core.ObjectLocation) int64 {
	if isCompressed(loc) {
		return loc.LogicalSize
	}
	if loc != nil && loc.Encrypted {
		return loc.PlaintextSize
	}
	if loc == nil {
		return 0
	}
	return loc.SizeBytes
}

// -------------------------------------------------------------------------
// COMPRESSED READ
// -------------------------------------------------------------------------

// compressedGetAttempt is the per-backend GET callback for a compressed copy.
// It issues no whole-object GET: the codec pulls frames through
// storedRangeFetcher as the client reads, so a ranged read fetches only the
// frames covering the range.
//
// The envelope signature at byte 0 is not checked, because the first bytes
// fetched are the seek table at the end. A row that disagrees with its bytes
// still fails: a copy it calls encrypted fails to decrypt, and one it calls
// plaintext fails to decode.
func (o *Manager) compressedGetAttempt(ctx context.Context, key, rangeHeader, beName string, backend s3be.ObjectBackend, loc *core.ObjectLocation) (readpath.ProbeResult[*s3be.GetObjectResult], error) {
	var fail readpath.ProbeResult[*s3be.GetObjectResult]

	if !o.core.Usage().WithinLimits(beName, getObjectOp, 0, 0) {
		return fail, fmt.Errorf("backend %s: %w", beName, readpath.ErrUsageLimitSkip)
	}
	if o.codec == nil {
		return fail, fmt.Errorf("backend %s: %w: object is compressed but no codec is configured",
			beName, core.ErrServiceUnavailable)
	}
	if err := core.ValidateEncryptionMetadata(loc); err != nil {
		telemetry.EncryptionFlagMismatchTotal.WithLabelValues("get").Inc()
		return fail, fmt.Errorf("backend %s: %w", beName, err)
	}

	fetcher := newStoredRangeFetcher(o.core, backend, o.encryptor, loc, core.StoragePath(key, loc.StorageKey), beName)
	reader, err := o.codec.DecompressRanged(ctx, fetcher, fetcher.compressedSize())
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
	// The response size is what the client was promised, which is the
	// denominator read amplification is measured against.
	telemetry.CompressionServedBytesTotal.Add(float64(r.Size))

	o.maybeWrapIntegrityReader(ctx, r, loc, key, beName, backend, r.ContentRange != "")
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
	size := logicalSize(loc)
	r := &s3be.GetObjectResult{Body: reader, Size: size}
	if attrs := fetcher.objectAttrs(); attrs != nil {
		r.ContentType, r.ETag = attrs.ContentType, attrs.ETag
		r.LastModified, r.Metadata = attrs.LastModified, attrs.Metadata
	}
	r.LastModified = resolveLastModified(r.LastModified, loc)
	if rangeHeader == "" {
		return r, nil
	}

	// An unsatisfiable range is served as the whole object. RFC 9110 lets a
	// server ignore a Range it cannot act on, and that is what the uncompressed
	// path does with one it cannot translate.
	start, end, ok := ParsePlaintextRange(rangeHeader, size)
	if !ok {
		return r, nil
	}
	if _, err := reader.Seek(start, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek to %d: %w", start, err)
	}
	r.Size = end - start + 1
	r.Body = ioutilx.ReadCloser(io.LimitReader(reader, r.Size), reader)
	r.ContentRange = fmt.Sprintf("bytes %d-%d/%d", start, end, size)
	return r, nil
}
