// -------------------------------------------------------------------------------
// Backend Runtime - Write Error Classification
//
// Author: Alex Freidah
//
// Translates write-path store errors into the S3 errors a client is answered
// with, and marks the span so traces show why a write was refused.
// -------------------------------------------------------------------------------

package infra

import (
	"errors"

	"go.opentelemetry.io/otel/trace"

	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// ClassifyWriteError translates store errors from write-path operations into
// S3-compatible errors and updates the tracing span. Increments the
// per-operation degraded-write-rejections counter when the failure is a
// DB-unavailable signal.
func (c *BackendRuntime) ClassifyWriteError(span trace.Span, op s3op.Operation, err error) error {
	switch {
	case errors.Is(err, core.ErrDBUnavailable):
		observe.MarkSpanError(span, "database unavailable")
		telemetry.DegradedWriteRejectionsTotal.WithLabelValues(op.String()).Inc()
		return core.ErrServiceUnavailable
	case errors.Is(err, core.ErrInsufficientStorage):
		observe.MarkSpanError(span, "insufficient storage")
		return err
	}
	observe.RecordSpanError(span, err)
	return err
}
