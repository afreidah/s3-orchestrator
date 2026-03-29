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

// TraceHandler wraps an slog.Handler and injects OpenTelemetry trace context
// (trace_id, span_id) into every log record that has an active span.
type TraceHandler struct {
	inner slog.Handler
}

// NewTraceHandler creates a handler that injects trace context into log records
// before delegating to the inner handler.
func NewTraceHandler(inner slog.Handler) *TraceHandler {
	return &TraceHandler{inner: inner}
}

// Enabled delegates to the inner handler.
func (h *TraceHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle adds trace_id and span_id attributes if the context carries an
// active span, then delegates to the inner handler.
func (h *TraceHandler) Handle(ctx context.Context, r slog.Record) error { //nolint:gocritic // slog.Handler interface requires value receiver
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
func (h *TraceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &TraceHandler{inner: h.inner.WithAttrs(attrs)}
}

// WithGroup returns a new TraceHandler wrapping the inner handler with a group.
func (h *TraceHandler) WithGroup(name string) slog.Handler {
	return &TraceHandler{inner: h.inner.WithGroup(name)}
}
