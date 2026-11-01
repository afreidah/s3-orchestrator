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
	"errors"
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

// ErrorWriter writes an error response in the format a route group's clients
// expect (S3-XML for s3api, JSON for the admin API). It must set the status
// code and write any headers before returning.
type ErrorWriter func(w http.ResponseWriter, status int, errCode, message string)

// PanicRecover returns middleware that recovers panics from h, translates
// them into a 500 response via writeErr, and records the panic in the log,
// the audit trail, a Prometheus counter, and the active span if any.
//
// route is the metric label and the audit event scope (e.g. "s3", "admin").
// It must come from a small fixed set, never from the request URL.
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
	err := panicError(rec)

	telemetry.HTTPPanicRecoveredTotal.WithLabelValues(route).Inc()

	log.ErrorContext(ctx, "panic recovered in HTTP handler",
		"route", route,
		"method", r.Method,
		"path", r.URL.Path,
		"panic", err.Error(),
		"stack", string(stack),
		"error", err,
	)

	audit.Log(ctx, "http.PanicRecovered",
		slog.String("route", route),
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.String("panic", err.Error()),
	)

	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetStatus(codes.Error, "panic recovered")
		span.RecordError(err)
	}

	writeErr(w, http.StatusInternalServerError, "InternalError", buildClientMessage(requestID))
}

// panicError returns the recovered value as an error, wrapping a non-error so
// it logs as a readable message. A nil panic never reaches here: since Go 1.21
// it arrives as a *runtime.PanicNilError.
func panicError(rec any) error {
	if err, ok := rec.(error); ok {
		return err
	}
	return errors.New(fmt.Sprint(rec))
}

// buildClientMessage builds the client-facing panic message. It echoes the
// request id for correlation but never the panic value.
func buildClientMessage(requestID string) string {
	if requestID == "" {
		return "An internal error occurred while processing the request."
	}
	return "An internal error occurred while processing the request. Request ID: " + requestID
}
