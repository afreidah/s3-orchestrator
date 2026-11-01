// -------------------------------------------------------------------------------
// Admin API - NDJSON Progress Streaming
//
// Author: Alex Freidah
//
// Shared helpers for streaming long-running admin operations as newline-
// delimited JSON. A handler opts in when the client accepts the stream content
// type, then emits one Event per line and flushes after each so the operator
// sees progress in real time rather than a single terminal payload.
// -------------------------------------------------------------------------------

package admin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// acceptsStream reports whether the client opted into NDJSON streaming via the
// Accept header.
func acceptsStream(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), adminstream.ContentType)
}

// passEndpoint is one on-demand pass as the transport sees it: the pass to run,
// how to word its stream, and how to render what it reported. Every pass
// endpoint is one of these, so the JSON and stream modes, the skip handling and
// the failure path are written once.
//
// body renders the endpoint's own response type and is also the stream's
// result fields, so the two modes cannot report different keys. It receives
// the outcome so a skipped pass renders the same shape with zero counts.
// skipIsError fails a skipped pass with its reason instead, for passes whose
// skip means the request cannot be served as asked. summary is the stream's
// result line; an empty one reads as "processed N".
type passEndpoint[R any] struct {
	op          string
	verb        string
	sequential  bool
	failMsg     string
	skipIsError bool
	run         func(context.Context, progress.Observer) (R, error)
	body        func(adminapi.Outcome, R) any
	summary     func(R) (processed int, line string)
}

// passOutcome is how one pass ended, decided once for both response modes.
// result is an adminstream outcome; message is the skip reason or what the
// caller is told of a failure; status is the code a failed JSON response
// carries.
type passOutcome struct {
	result  string
	message string
	status  int
}

// servePass runs one pass. Streams per-item NDJSON progress when the client
// accepts the stream content type; otherwise returns a single JSON result. The
// pass runs under the request context, so a disconnecting caller stops it.
func (h *Handler) servePass[R any](w http.ResponseWriter, r *http.Request, ep passEndpoint[R]) {
	if acceptsStream(r) {
		h.streamPass(w, r, ep)
		return
	}

	res, err := ep.run(r.Context(), nil)
	switch o := ep.settle(r.Context(), h.log, err); o.result {
	case adminstream.OutcomeOK:
		httputil.WriteJSON(w, http.StatusOK, ep.body(adminapi.Outcome{Status: statusOK}, res))
	case adminstream.OutcomeSkipped:
		var zero R
		httputil.WriteJSON(w, http.StatusOK, ep.body(adminapi.Outcome{Status: statusSkipped, Reason: o.message}, zero))
	default:
		httputil.WriteJSONError(w, o.status, o.message)
	}
}

// settle decides how a pass that returned err ended. A failure is logged here,
// so the caller is told only failMsg; a skip the endpoint refuses fails with
// its reason, which is the caller's to act on.
func (ep *passEndpoint[R]) settle(ctx context.Context, log *slog.Logger, err error) passOutcome {
	reason, skipped := ops.SkipReason(err)
	switch {
	case skipped && ep.skipIsError:
		return passOutcome{result: adminstream.OutcomeFailed, message: reason, status: http.StatusBadRequest}
	case skipped:
		return passOutcome{result: adminstream.OutcomeSkipped, message: reason}
	case err != nil:
		log.ErrorContext(ctx, ep.failMsg, logfmt.Err(err))
		return passOutcome{result: adminstream.OutcomeFailed, message: ep.failMsg, status: http.StatusInternalServerError}
	}
	return passOutcome{result: adminstream.OutcomeOK}
}

// resultFields flattens a response body into the stream result's fields,
// leaving out the outcome, which the result event reports itself.
func resultFields(body any) map[string]any {
	data, err := json.Marshal(body)
	if err != nil {
		return nil
	}
	var fields map[string]any
	if json.Unmarshal(data, &fields) != nil {
		return nil
	}
	delete(fields, "status")
	delete(fields, "reason")
	return fields
}

// streamPass runs a pass as an NDJSON step stream: a start event, a
// step_start/step_end pair per item via the progress observer, and a terminal
// result settled the way JSON mode settles it. verb prefixes each item line
// ("hashing", "reconciling", ...).
//
// sequential controls rendering. A sequential op emits a step_start (the client
// prints the "<verb> <item> ....." prefix) and a bare step_end that completes
// the line. A concurrent op would interleave live prefixes, so each finished
// item emits one labeled step_end instead. emit is mutex-guarded, so a
// concurrent observer is safe.
func (h *Handler) streamPass[R any](w http.ResponseWriter, r *http.Request, ep passEndpoint[R]) {
	emit := newEventStream(w)
	emit(adminstream.Event{Kind: adminstream.KindStart, Op: ep.op})
	start := time.Now()

	observer := func(s progress.Step) {
		switch {
		case s.Phase == progress.PhaseStart && ep.sequential:
			emit(adminstream.Event{Kind: adminstream.KindStepStart, Message: ep.verb + " " + s.Label})
		case s.Phase == progress.PhaseEnd && ep.sequential:
			emit(adminstream.Event{Kind: adminstream.KindStepEnd, Outcome: s.Status, DurationMs: s.Duration.Milliseconds()})
		case s.Phase == progress.PhaseEnd:
			emit(adminstream.Event{Kind: adminstream.KindStepEnd, Message: ep.verb + " " + s.Label, Outcome: s.Status, DurationMs: s.Duration.Milliseconds()})
		}
	}

	res, err := ep.run(r.Context(), observer)
	o := ep.settle(r.Context(), h.log, err)
	result := adminstream.Event{Kind: adminstream.KindResult, Outcome: o.result}
	switch o.result {
	case adminstream.OutcomeOK:
		result.Processed, result.Message = ep.summary(res)
		result.DurationMs = time.Since(start).Milliseconds()
		result.Fields = resultFields(ep.body(adminapi.Outcome{Status: statusOK}, res))
	case adminstream.OutcomeSkipped:
		result.Message = o.message
	default:
		result.Error = o.message
	}
	emit(result)
}

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// newEventStream returns an emit function that writes one Event per line and
// flushes after each. It clears the write deadline first, since the absolute
// server.write_timeout would otherwise reset a long pass mid-stream. After a
// failed write it writes nothing more: the connection is gone, and the pass
// itself stops with the request context.
func newEventStream(w http.ResponseWriter) func(adminstream.Event) {
	clearWriteDeadline(w)
	w.Header().Set("Content-Type", adminstream.ContentType)
	enc := json.NewEncoder(w)
	flusher, canFlush := w.(http.Flusher)
	var mu sync.Mutex
	var broken bool
	return func(e adminstream.Event) {
		mu.Lock()
		defer mu.Unlock()
		if broken {
			return
		}
		if enc.Encode(e) != nil {
			broken = true
			return
		}
		if canFlush {
			flusher.Flush()
		}
	}
}

// clearWriteDeadline lifts the server's write timeout for one response. A writer
// without deadline support has no timeout to lift, so its error is ignored.
func clearWriteDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
}
