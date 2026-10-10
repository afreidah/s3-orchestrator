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

	"github.com/afreidah/s3-orchestrator/internal/ops"
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
// structured detail for JSON mode. A skip comes back as an ops skip error.
type stepResult struct {
	Processed int
	Summary   string
	Fields    map[string]any
}

// streamSteps runs a long-running operation as an NDJSON step stream: a start
// event, a step_start/step_end pair per item via the progress observer, and a
// terminal result. verb prefixes each item line ("hashing", "reconciling", ...).
//
// sequential controls rendering. A sequential op emits a step_start (the client
// prints the "<verb> <item> ....." prefix) and a bare step_end that completes
// the line. A concurrent op would interleave live prefixes, so each finished
// item emits one labeled step_end instead. emit is mutex-guarded, so a
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
	reason, skipped := ops.SkipReason(err)
	switch {
	case skipped:
		emit(adminstream.Event{Kind: adminstream.KindResult, Outcome: adminstream.OutcomeSkipped, Message: reason})
	case err != nil:
		emit(adminstream.Event{Kind: adminstream.KindResult, Outcome: adminstream.OutcomeFailed, Error: err.Error()})
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

// newEventStream returns an emit function that writes one Event per line and
// flushes after each. It clears the write deadline first, since the absolute
// server.write_timeout would otherwise reset a long pass mid-stream.
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

// clearWriteDeadline lifts the server's write timeout for one response. A writer
// without deadline support has no timeout to lift, so its error is ignored.
func clearWriteDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
}
