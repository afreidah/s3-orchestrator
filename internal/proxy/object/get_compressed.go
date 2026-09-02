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
	"time"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/readpath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/ioutilx"
)

// isCompressed reports whether a copy's stored bytes were encoded. An empty
// algorithm is the store's own way of saying the bytes are verbatim, so this
// never consults a separate flag that could disagree with it.
func isCompressed(loc *core.ObjectLocation) bool {
	return loc != nil && loc.CompressionAlgorithm != ""
}

// resolveLastModified reports the Last-Modified a read should answer with,
// given what the backend said and the row for the copy that served it.
//
// The stored time wins whenever there is a row. It is the object's write time,
// identical on every copy, where a backend reports when it received the copy
// that answered - so preferring the backend makes an unmodified object change
// its Last-Modified on failover, and If-Modified-Since and If-Range compare
// against a value that moves. Only the degraded path, serving with the database
// down and no row to consult, falls back to the backend's.
//
// A backend that reports nothing is the case this started as: the transport
// drops an empty Last-Modified and clients like Tempo's blocklist poller reject
// the response outright. A stored time that is somehow zero therefore still
// yields to the backend's, so the guarantee that every response carries a
// timestamp holds no matter which side is missing one.
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

// compressedGetAttempt is the per-backend GET callback for a compressed copy.
//
// It issues no whole-object GET. The codec drives the read instead, pulling
// frames through storedRangeFetcher as the client consumes the body, so a
// ranged read fetches the frames covering that range and nothing else.
//
// The stored envelope is not inspected the way an uncompressed read inspects
// it: the first bytes fetched are the seek table at the end of the object, not
// byte 0 where the signature lives. A row that disagrees with its bytes still
// fails, just later and by a different route - a copy the row calls encrypted
// fails to decrypt, and one it calls plaintext fails to decode.
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

	fetcher := newStoredRangeFetcher(o.core, backend, o.encryptor, loc, key, beName)
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
// applying the client's range in logical coordinates.
//
// The metadata comes off the fetcher rather than a GET of our own: building the
// reader already read the seek table, so a response has arrived and its headers
// are the ones the object carries.
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
