// -------------------------------------------------------------------------------
// HTTP Panic Recovery Middleware
//
// Author: Alex Freidah
//
// Catches panics inside HTTP handlers and turns them into a structured 500
// response that operators can correlate with logs, traces, and the audit
// stream. The net/http server has its own recover but it closes the
// connection without writing a response, prints the stack trace to stderr
// in free-form text, and produces no Prometheus signal. Routes that wrap
// themselves with PanicRecover get an S3-aware (or admin-aware, or UI-
// aware) 500 with the request id echoed back, an slog.ErrorContext line
// scoped to component=httputil, a span error if there is an active OTel
// span, an "http.PanicRecovered" audit event, and an
// s3o_http_panic_recovered_total{route} increment.
// -------------------------------------------------------------------------------

package httputil

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// ErrorWriter writes a route-appropriate 500 response. The middleware
// stays format-agnostic - each route group passes a writer that matches
// what its clients expect (S3-XML for the s3api surface, JSON for the
// admin API, plaintext for the UI). The writer MUST set the status code
// and write any headers before returning; the middleware only invokes
// it once per recovered panic.
type ErrorWriter func(w http.ResponseWriter, status int, errCode, message string)

// PanicRecover returns middleware that recovers panics from h, translates
// them into a 500 response via writeErr, and emits the three observability
// signals (slog.ErrorContext + audit + Prometheus counter) plus OTel span
// error recording when a span is active on the request context.
//
// route is the metric label and the audit event scope (e.g. "s3", "admin",
// "ui"). It must come from a small fixed set per the cardinality rules in
// docs/style-guide.md - never derive it from the request URL.
func PanicRecover(route string, writeErr ErrorWriter) func(http.Handler) http.Handler {
	log := slog.Default().With(logfmt.Component("httputil"))
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				handleRecoveredPanic(r, w, log, route, writeErr, rec)
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// handleRecoveredPanic is the slow path that fires only when a handler
// panicked. Pulled out of the defer closure so the hot non-panic path
// stays a single nil check plus a return.
func handleRecoveredPanic(r *http.Request, w http.ResponseWriter, log *slog.Logger, route string, writeErr ErrorWriter, rec any) {
	ctx := r.Context()
	requestID := audit.RequestID(ctx)
	stack := debug.Stack()

	telemetry.HTTPPanicRecoveredTotal.WithLabelValues(route).Inc()

	log.ErrorContext(ctx, "panic recovered in HTTP handler",
		"route", route,
		"method", r.Method,
		"path", r.URL.Path,
		"panic", panicMessage(rec),
		"stack", string(stack),
		logfmt.Err(panicError(rec)),
	)

	audit.Log(ctx, "http.PanicRecovered",
		slog.String("route", route),
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.String("panic", panicMessage(rec)),
	)

	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetStatus(codes.Error, "panic recovered")
		span.RecordError(panicError(rec))
	}

	writeErr(w, http.StatusInternalServerError, "InternalError", buildClientMessage(requestID))
}

// panicError wraps the recovered value as an error so logfmt.Err and
// span.RecordError see the same payload. A bare error keeps its type;
// any other value (string, runtime.Error subtype, custom struct) is
// wrapped via panicValueError so the JSON handler still serialises a
// human-readable message instead of "{}".
func panicError(rec any) error {
	if err, ok := rec.(error); ok {
		return err
	}
	return panicValueError{value: rec}
}

// panicMessage returns a printable representation for the structured
// "panic" log key and the audit "panic" attribute. Kept separate from
// panicError so the audit and slog forms stay consistent even when the
// recovered value is not an error type.
func panicMessage(rec any) string {
	if err, ok := rec.(error); ok {
		return err.Error()
	}
	return panicValueError{value: rec}.Error()
}

// panicValueError lifts a non-error recovered value into the error
// interface. Used for the common case where a handler panics with a
// string or a runtime.Error that does not satisfy error directly.
type panicValueError struct {
	value any
}

// Error renders the wrapped value via the default fmt verb so any
// recovered shape produces a stable string.
func (p panicValueError) Error() string {
	return formatPanicValue(p.value)
}

// formatPanicValue keeps the panic-to-string conversion in one place so
// log lines and the response body stay consistent.
func formatPanicValue(v any) string {
	if v == nil {
		return "<nil panic>"
	}
	if s, ok := v.(string); ok {
		return s
	}
	if err, ok := v.(error); ok {
		return err.Error()
	}
	return fmt.Sprintf("%v", v)
}

// buildClientMessage constructs the response message returned to the
// client. The request id is echoed so customer support can correlate a
// failure report with the recovery log line. The panic value itself is
// deliberately not exposed - it can leak stack frames and internal
// names that have no business reaching an S3 client.
func buildClientMessage(requestID string) string {
	if requestID == "" {
		return "An internal error occurred while processing the request."
	}
	return "An internal error occurred while processing the request. Request ID: " + requestID
}
