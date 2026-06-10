// -------------------------------------------------------------------------------
// Admin Stream - NDJSON Progress Event Contract
//
// Author: Alex Freidah
//
// Wire contract shared by the admin API server and the adminctl client for
// streaming long-running operations. The server emits one Event per line
// (newline-delimited JSON) with the ContentType media type; the client decodes
// line by line and renders incremental progress. A leaf package with no
// app-specific dependencies so both the transport and CLI layers can import it
// without a cycle.
// -------------------------------------------------------------------------------

package adminstream

// ContentType is the media type for a newline-delimited JSON admin event
// stream. The client sets it in the Accept header to opt into streaming; the
// server sets it on the response when it streams.
const ContentType = "application/x-ndjson"

// Event kinds. Every streamed line carries exactly one Kind.
const (
	KindStart     = "start"      // first line: operation accepted and beginning
	KindProgress  = "progress"   // emitted as work advances (single-line update)
	KindStepStart = "step_start" // a named unit of work began (no newline yet)
	KindStepEnd   = "step_end"   // that unit finished (completes the line)
	KindResult    = "result"     // final line: terminal outcome
)

// Result outcomes carried on a KindResult event.
const (
	OutcomeOK      = "ok"
	OutcomeSkipped = "skipped"
	OutcomeFailed  = "failed"
)

// Event is one line of an admin operation stream. Each line is a self-contained
// JSON object; consumers switch on Kind. Counters carry incremental progress;
// Fields carries operation-specific detail (a final summary, per-backend rows)
// without growing the schema per operation.
type Event struct {
	Kind       string         `json:"event"`
	Op         string         `json:"op,omitempty"`          // start: operation name
	Message    string         `json:"message,omitempty"`     // progress label or skip reason
	Processed  int            `json:"processed,omitempty"`   // cumulative items handled
	Outcome    string         `json:"outcome,omitempty"`     // result: ok|skipped|failed
	Error      string         `json:"error,omitempty"`       // result: failure detail
	DurationMs int64          `json:"duration_ms,omitempty"` // result: wall-clock elapsed
	Fields     map[string]any `json:"fields,omitempty"`      // operation-specific detail
}
