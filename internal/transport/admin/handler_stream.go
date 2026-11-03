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
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/progress"
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

// servePass runs one pass. Streams per-item NDJSON progress when the client
// accepts the stream content type; otherwise returns a single JSON result. The
// pass runs under the request context, so a disconnecting caller stops it.
func (h *Handler) servePass[R any](w http.ResponseWriter, r *http.Request, p *Pass[R]) {
	if acceptsStream(r) {
		h.streamPass(w, r, p)
		return
	}
	if rep := p.Run(r.Context(), h.log); rep.Body != nil {
		httputil.WriteJSON(w, http.StatusOK, rep.Body)
	} else {
		httputil.WriteJSONError(w, rep.Status, rep.Message)
	}
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
func (h *Handler) streamPass[R any](w http.ResponseWriter, r *http.Request, p *Pass[R]) {
	emit := newEventStream(w)
	emit(adminstream.Event{Kind: adminstream.KindStart, Op: p.op})
	start := time.Now()

	observer := func(s progress.Step) {
		switch {
		case s.Phase == progress.PhaseStart && p.sequential:
			emit(adminstream.Event{Kind: adminstream.KindStepStart, Message: p.verb + " " + s.Label})
		case s.Phase == progress.PhaseEnd && p.sequential:
			emit(adminstream.Event{Kind: adminstream.KindStepEnd, Outcome: s.Status, DurationMs: s.Duration.Milliseconds()})
		case s.Phase == progress.PhaseEnd:
			emit(adminstream.Event{Kind: adminstream.KindStepEnd, Message: p.verb + " " + s.Label, Outcome: s.Status, DurationMs: s.Duration.Milliseconds()})
		}
	}

	rep, res := p.exec(r.Context(), h.log, observer)
	result := adminstream.Event{Kind: adminstream.KindResult, Outcome: rep.Result}
	switch rep.Result {
	case adminstream.OutcomeOK:
		result.Processed, result.Message = p.summary(res)
		result.DurationMs = time.Since(start).Milliseconds()
		result.Fields = resultFields(rep.Body)
	case adminstream.OutcomeSkipped:
		result.Message = rep.Message
	default:
		result.Error = rep.Message
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
