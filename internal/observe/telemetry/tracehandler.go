// -------------------------------------------------------------------------------
// Trace Handler - slog Handler with OpenTelemetry Context Injection
//
// Author: Alex Freidah
//
// Wraps any slog.Handler to automatically inject trace_id and span_id from
// the context into every log record. Enables trace-to-log correlation in
// Grafana (Tempo -> Loki) without requiring callers to manually add trace
// attributes to each log call.
// -------------------------------------------------------------------------------

package telemetry

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// traceHandler wraps an slog.Handler and injects OpenTelemetry trace context
// (trace_id, span_id) into every log record that has an active span.
type traceHandler struct {
	inner slog.Handler
}

// newTraceHandler creates a handler that injects trace context into log records
// before delegating to the inner handler.
func newTraceHandler(inner slog.Handler) *traceHandler {
	return &traceHandler{inner: inner}
}

// Enabled delegates to the inner handler.
func (h *traceHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle adds trace_id and span_id attributes if the context carries an
// active span, then delegates to the inner handler.
func (h *traceHandler) Handle(ctx context.Context, r slog.Record) error { //nolint:gocritic // slog.Handler interface requires value receiver
	span := trace.SpanFromContext(ctx)
	if span.SpanContext().IsValid() {
		r.AddAttrs(
			slog.String("trace_id", span.SpanContext().TraceID().String()),
			slog.String("span_id", span.SpanContext().SpanID().String()),
		)
	}
	return h.inner.Handle(ctx, r)
}

// WithAttrs returns a new TraceHandler wrapping the inner handler with attrs.
func (h *traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &traceHandler{inner: h.inner.WithAttrs(attrs)}
}

// WithGroup returns a new TraceHandler wrapping the inner handler with a group.
func (h *traceHandler) WithGroup(name string) slog.Handler {
	return &traceHandler{inner: h.inner.WithGroup(name)}
}
