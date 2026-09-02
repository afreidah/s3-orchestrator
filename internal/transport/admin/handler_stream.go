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
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// acceptsStream reports whether the client opted into NDJSON streaming via the
// Accept header.
func acceptsStream(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), adminstream.ContentType)
}

// stepResult is what a streamed operation reports on completion. Summary is the
// human-readable result line (empty falls back to "processed N"); Fields carries
// structured detail for JSON mode; a non-empty Skipped reports a skip.
type stepResult struct {
	Processed int
	Summary   string
	Fields    map[string]any
	Skipped   string
}

// streamSteps runs a long-running operation as an NDJSON step stream: a start
// event, a step_start/step_end pair per item via the progress observer, and a
// terminal result. verb prefixes each item line ("hashing", "reconciling", ...).
//
// sequential controls rendering. A sequential op processes one item at a time,
// so each item gets a step_start (the client prints the "<verb> <item> ....."
// prefix immediately) and a bare step_end (the client completes the line with
// the status). A concurrent op runs many items at once, where a live prefix
// would interleave, so each finished item emits a single labeled step_end the
// client renders as one complete line. emit is mutex-guarded either way, so a
// concurrent observer is safe.
func (h *Handler) streamSteps(w http.ResponseWriter, op, verb string, sequential bool, run func(progress.Observer) (stepResult, error)) {
	emit := newEventStream(w)
	emit(adminstream.Event{Kind: adminstream.KindStart, Op: op})
	start := time.Now()

	observer := func(s progress.Step) {
		switch {
		case s.Phase == progress.PhaseStart && sequential:
			emit(adminstream.Event{Kind: adminstream.KindStepStart, Message: verb + " " + s.Label})
		case s.Phase == progress.PhaseEnd && sequential:
			emit(adminstream.Event{Kind: adminstream.KindStepEnd, Outcome: s.Status, DurationMs: s.Duration.Milliseconds()})
		case s.Phase == progress.PhaseEnd:
			emit(adminstream.Event{Kind: adminstream.KindStepEnd, Message: verb + " " + s.Label, Outcome: s.Status, DurationMs: s.Duration.Milliseconds()})
		}
	}

	res, err := run(observer)
	switch {
	case err != nil:
		emit(adminstream.Event{Kind: adminstream.KindResult, Outcome: adminstream.OutcomeFailed, Error: err.Error()})
	case res.Skipped != "":
		emit(adminstream.Event{Kind: adminstream.KindResult, Outcome: adminstream.OutcomeSkipped, Message: res.Skipped})
	default:
		emit(adminstream.Event{
			Kind:       adminstream.KindResult,
			Outcome:    adminstream.OutcomeOK,
			Processed:  res.Processed,
			Message:    res.Summary,
			DurationMs: time.Since(start).Milliseconds(),
			Fields:     res.Fields,
		})
	}
}

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// newEventStream sets the stream content type on w and returns an emit function
// that encodes one Event per line and flushes after each so progress reaches
// the client immediately. The flush is best-effort: a ResponseWriter that does
// not implement http.Flusher still receives every line, just buffered.
//
// The write deadline is cleared first. server.write_timeout bounds an ordinary
// response, and a stream is not one: it stays open for the whole pass, which on
// a real fleet outlasts any timeout worth applying to a request that is meant
// to finish. The deadline is absolute rather than idle-based, so flushing
// progress does not hold it off - it fires mid-pass, the server resets the
// stream, and the client reports INTERNAL_ERROR having seen the work running
// fine moments earlier.
//
// Cleared per response rather than by widening the server timeout, so every
// request that is not a stream keeps the protection. Cancellation is unaffected:
// it arrives through the request context when the client disconnects, which is
// what the passes already stop on.
func newEventStream(w http.ResponseWriter) func(adminstream.Event) {
	clearWriteDeadline(w)
	w.Header().Set("Content-Type", adminstream.ContentType)
	enc := json.NewEncoder(w)
	flusher, canFlush := w.(http.Flusher)
	var mu sync.Mutex
	return func(e adminstream.Event) {
		mu.Lock()
		defer mu.Unlock()
		_ = enc.Encode(e)
		if canFlush {
			flusher.Flush()
		}
	}
}

// clearWriteDeadline lifts the server's write timeout for one response.
//
// Best-effort by design: a ResponseWriter that does not support deadlines
// (httptest's recorder, a middleware that wraps without forwarding
// Unwrap) returns http.ErrNotSupported, and there is nothing to lift there
// anyway. Failing the stream over it would break the handler for exactly the
// writers that were never subject to the deadline.
func clearWriteDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
}
