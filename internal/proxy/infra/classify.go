// -------------------------------------------------------------------------------
// Backend Runtime - Write Error Classification
//
// Author: Alex Freidah
//
// Maps store errors from the write path to the S3 errors a client sees, and
// records them on the tracing span.
// -------------------------------------------------------------------------------

package infra

import (
	"errors"

	"go.opentelemetry.io/otel/trace"

	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// ClassifyWriteError translates store errors from write-path operations into
// S3-compatible errors and updates the tracing span. Increments the
// per-operation degraded-write-rejections counter when the failure is a
// DB-unavailable signal.
func (c *BackendRuntime) ClassifyWriteError(span trace.Span, operation string, err error) error {
	if errors.Is(err, core.ErrDBUnavailable) {
		observe.MarkSpanError(span, "database unavailable")
		telemetry.DegradedWriteRejectionsTotal.WithLabelValues(operation).Inc()
		return core.ErrServiceUnavailable
	}
	if errors.Is(err, core.ErrNoSpaceAvailable) {
		observe.MarkSpanError(span, "insufficient storage")
		return core.ErrInsufficientStorage
	}
	observe.RecordSpanError(span, err)
	return err
}
