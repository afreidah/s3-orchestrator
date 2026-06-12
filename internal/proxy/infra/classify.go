// -------------------------------------------------------------------------------
// Error Classifier - Store-Side Write-Path Error Translation
//
// Author: Alex Freidah
//
// Translates store-side errors surfaced by write-path operations into
// S3-compatible errors, updating the tracing span and emitting the
// matching degraded-write-rejection telemetry as a side effect. A
// stateless helper, packaged as a type so future variants (different
// span semantics, additional error mappings) can be swapped at the
// composition layer without changing every consumer.
// -------------------------------------------------------------------------------

package infra

import (
	"errors"

	"go.opentelemetry.io/otel/trace"

	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// errorClassifier is a stateless helper that maps store errors to S3
// errors. Held as a struct (rather than free functions) so the
// composition layer in *BackendRuntime can swap it for a different policy in
// tests without touching callers.
type errorClassifier struct{}

// newErrorClassifier returns the default classifier.
func newErrorClassifier() *errorClassifier {
	return &errorClassifier{}
}

// ClassifyWriteError translates store errors from write-path
// operations into S3-compatible errors and updates the tracing span.
// Increments the per-operation degraded-write-rejections counter when
// the failure is a DB-unavailable signal.
func (c *errorClassifier) ClassifyWriteError(span trace.Span, operation string, err error) error {
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
