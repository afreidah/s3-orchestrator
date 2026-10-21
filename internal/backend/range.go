// -------------------------------------------------------------------------------
// Backend - Range Emulation for Backends That Ignore Range
//
// Author: Alex Freidah
//
// A backend that does not support Range answers a ranged GET with 200, the
// whole object and no Content-Range. Every ranged caller (encrypted reads,
// compressed frame fetches, import classification) would otherwise treat the
// start of the object as the range it asked for. The cut happens here instead,
// so callers always receive the bytes they requested.
// -------------------------------------------------------------------------------

package backend

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/util/byterange"
	"github.com/afreidah/s3-orchestrator/internal/util/ioutilx"
)

// rangeNotSatisfiableError reports a range that addresses no byte of the
// object, built locally when the backend ignored Range. It carries the same
// 416 status a backend that honours Range answers with, so callers cannot tell
// the two apart.
type rangeNotSatisfiableError struct {
	header string
	size   int64
}

// Error describes the unsatisfiable range and the object's size.
func (e *rangeNotSatisfiableError) Error() string {
	return fmt.Sprintf("range %q not satisfiable for a %d-byte object", e.header, e.size)
}

// HTTPStatusCode reports 416, matching the status a backend that honours Range
// answers with.
func (e *rangeNotSatisfiableError) HTTPStatusCode() int {
	return http.StatusRequestedRangeNotSatisfiable
}

// applyRange cuts the requested range out of a whole-object response. res.Size
// must be the full object's length, as a 200 reports it. On success res carries
// the range's body, Size and Content-Range, and WireSize counts every byte read
// from the backend to reach the end of the range. On error res.Body is closed.
func applyRange(res *GetObjectResult, header string) error {
	start, end, ok := byterange.Parse(header, res.Size)
	if !ok {
		_ = res.Body.Close()
		return &rangeNotSatisfiableError{header: header, size: res.Size}
	}
	body, err := ioutilx.Window(res.Body, start, end-start+1)
	if err != nil {
		_ = res.Body.Close()
		return fmt.Errorf("skip to range start: %w", err)
	}
	res.Body = ioutilx.ReadCloser(body, res.Body)
	res.ContentRange = byterange.ContentRange(start, end, res.Size)
	res.WireSize = end + 1
	res.Size = end - start + 1
	return nil
}

// emulateRange applies the range when a ranged GET came back without
// Content-Range. It counts every occurrence and warns once per backend, so an
// operator learns which backend ignores Range without a log line per read.
func (b *S3Backend) emulateRange(ctx context.Context, res *GetObjectResult, header string) error {
	if header == "" || res.ContentRange != "" {
		return nil
	}
	telemetry.BackendRangeIgnoredTotal.WithLabelValues(b.name).Inc()
	b.rangeIgnored.Do(func() {
		slog.Default().With(logfmt.Component("backend")).WarnContext(ctx,
			"backend ignored Range and returned the whole object; ranged reads will download from the start of the object",
			"backend", b.name)
	})
	return applyRange(res, header)
}
