// -------------------------------------------------------------------------------
// Log Buffer - In-Memory Ring Buffer for Structured Logs
//
// Author: Alex Freidah
//
// Thread-safe circular buffer that captures slog output for the operator
// dashboard. A TeeHandler fans out log records to both the standard JSON
// handler (stdout) and this buffer. The buffer holds the most recent 5,000
// entries (~5 MB worst case) and supports filtered queries by level, time,
// and component.
// -------------------------------------------------------------------------------

package telemetry

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
)

// logBufferCapacity is the maximum number of log entries retained.
const logBufferCapacity = 5000

// -------------------------------------------------------------------------
// LOG ENTRY
// -------------------------------------------------------------------------

// LogEntry is a single structured log record stored in the buffer.
type LogEntry struct {
	Time    time.Time      `json:"time"`
	Level   slog.Level     `json:"level"`
	Message string         `json:"message"`
	Attrs   map[string]any `json:"attrs,omitempty"`
}

// -------------------------------------------------------------------------
// QUERY OPTIONS
// -------------------------------------------------------------------------

// LogQueryOpts controls filtering when reading from the buffer.
type LogQueryOpts struct {
	MinLevel  slog.Level // minimum severity (zero value is slog.LevelInfo, which excludes DEBUG)
	Since     time.Time  // only entries after this time
	Before    time.Time  // only entries before this time
	Limit     int        // max entries to return (0 = all)
	Component string     // filter by "component" attribute value
}

// -------------------------------------------------------------------------
// LOG BUFFER
// -------------------------------------------------------------------------

// LogBuffer is a thread-safe circular buffer of log entries.
type LogBuffer struct {
	mu      sync.RWMutex
	entries []LogEntry
	pos     int  // next write position
	full    bool // whether the buffer has wrapped
}

// NewLogBuffer creates a ring buffer with the default capacity.
func NewLogBuffer() *LogBuffer {
	return &LogBuffer{
		entries: make([]LogEntry, logBufferCapacity),
	}
}

// Add appends a log entry to the buffer, overwriting the oldest entry
// when the buffer is full.
func (b *LogBuffer) Add(entry LogEntry) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.entries[b.pos] = entry
	b.pos++
	if b.pos >= len(b.entries) {
		b.pos = 0
		b.full = true
	}
}

// Entries returns buffered log entries matching the query options.
// Results are returned in chronological order (oldest first). Filtering runs
// outside the lock so slow queries do not block Add.
func (b *LogBuffer) Entries(opts *LogQueryOpts) []LogEntry {
	snapshot := b.snapshotEntries()
	if len(snapshot) == 0 {
		return nil
	}
	result := filterEntries(snapshot, opts)
	if opts.Limit > 0 && len(result) > opts.Limit {
		result = result[len(result)-opts.Limit:]
	}
	return result
}

// snapshotEntries acquires the read lock just long enough to copy the
// ring-buffer contents into a slice, then releases it. Filtering happens
// on the snapshot outside the lock so concurrent Add calls aren't blocked
// by slow dashboard queries.
func (b *LogBuffer) snapshotEntries() []LogEntry {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if !b.full {
		return slices.Clone(b.entries[:b.pos])
	}
	return append(slices.Clone(b.entries[b.pos:]), b.entries[:b.pos]...)
}

// filterEntries applies the query filters (time range, min level,
// component) to the snapshot. Pure function  -  no locking.
func filterEntries(snapshot []LogEntry, opts *LogQueryOpts) []LogEntry {
	result := make([]LogEntry, 0, len(snapshot))
	for _, e := range snapshot {
		if !opts.Since.IsZero() && e.Time.Before(opts.Since) {
			continue
		}
		if !opts.Before.IsZero() && !e.Time.Before(opts.Before) {
			continue
		}
		if e.Level < opts.MinLevel {
			continue
		}
		if opts.Component != "" {
			if comp, ok := e.Attrs["component"]; !ok || comp != opts.Component {
				continue
			}
		}
		result = append(result, e)
	}
	return result
}

// -------------------------------------------------------------------------
// TEE HANDLER
// -------------------------------------------------------------------------

// TeeHandler is an slog.Handler that writes each log record to a primary
// handler (typically JSON to stdout) and also captures it in a LogBuffer.
type TeeHandler struct {
	primary slog.Handler
	buf     *LogBuffer
	attrs   []slog.Attr
	groups  []string
}

// NewLogHandler builds the daemon's handler chain: each record at or above
// level is captured in buf and written to w as JSON, carrying the trace and
// span IDs from its context.
func NewLogHandler(w io.Writer, level slog.Leveler, buf *LogBuffer) slog.Handler {
	return NewTeeHandler(NewTraceHandler(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})), buf)
}

// NewTeeHandler creates a handler that fans out to both the primary handler
// and the ring buffer.
func NewTeeHandler(primary slog.Handler, buf *LogBuffer) *TeeHandler {
	return &TeeHandler{
		primary: primary,
		buf:     buf,
	}
}

// Enabled reports whether the handler handles records at the given level.
// Delegates to the primary handler.
func (h *TeeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.primary.Enabled(ctx, level)
}

// Handle renders the record's error attributes as strings once, then writes
// the result to the primary handler and captures it in the buffer. JSON
// encoding would otherwise render many errors as "{}".
func (h *TeeHandler) Handle(ctx context.Context, r slog.Record) error { //nolint:gocritic // slog.Handler interface requires value receiver
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	attrs := make(map[string]any)
	prefix := groupPrefix(h.groups)
	for _, a := range h.attrs {
		attrs[prefix+a.Key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		a = logfmt.TransformAttr(a)
		out.AddAttrs(a)
		attrs[prefix+a.Key] = a.Value.Any()
		return true
	})

	err := h.primary.Handle(ctx, out)

	// Captured regardless of primary handler errors.
	entry := LogEntry{Time: r.Time, Level: r.Level, Message: r.Message}
	if len(attrs) > 0 {
		entry.Attrs = attrs
	}
	h.buf.Add(entry)
	return err
}

// WithAttrs returns a new TeeHandler with the given attributes added, their
// errors rendered as strings for both the primary handler and the buffer.
func (h *TeeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = logfmt.TransformAttr(a)
	}
	return &TeeHandler{
		primary: h.primary.WithAttrs(out),
		buf:     h.buf,
		attrs:   append(slices.Clone(h.attrs), out...),
		groups:  slices.Clone(h.groups),
	}
}

// WithGroup returns a new TeeHandler with the given group name.
func (h *TeeHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &TeeHandler{
		primary: h.primary.WithGroup(name),
		buf:     h.buf,
		attrs:   slices.Clone(h.attrs),
		groups:  append(slices.Clone(h.groups), name),
	}
}

// groupPrefix builds a dotted prefix from the current group stack.
func groupPrefix(groups []string) string {
	if len(groups) == 0 {
		return ""
	}
	return strings.Join(groups, ".") + "."
}
