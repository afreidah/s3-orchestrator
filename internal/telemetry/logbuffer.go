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

// Package telemetry provides Prometheus metrics registration and OpenTelemetry
// tracing initialization for the S3 orchestrator.
package telemetry

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
)

// logBufferCapacity is the maximum number of log entries retained.
const logBufferCapacity = 5000

// -------------------------------------------------------------------------
// LOG ENTRY
// -------------------------------------------------------------------------

// LogEntry is a single structured log record stored in the buffer.
type LogEntry struct {
	Time    time.Time      `json:"time"`
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Attrs   map[string]any `json:"attrs,omitempty"`
}

// -------------------------------------------------------------------------
// QUERY OPTIONS
// -------------------------------------------------------------------------

// LogQueryOpts controls filtering when reading from the buffer.
type LogQueryOpts struct {
	MinLevel  slog.Level // minimum severity (default 0 = DEBUG)
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
// Results are returned in chronological order (oldest first).
func (b *LogBuffer) Entries(opts *LogQueryOpts) []LogEntry {
	b.mu.RLock()
	defer b.mu.RUnlock()

	// Determine the range of valid entries.
	var count int
	if b.full {
		count = len(b.entries)
	} else {
		count = b.pos
	}
	if count == 0 {
		return nil
	}

	// Pre-parse the min level string for comparison.
	minLevel := opts.MinLevel

	result := make([]LogEntry, 0, count)
	for i := range count {
		var idx int
		if b.full {
			idx = (b.pos + i) % len(b.entries)
		} else {
			idx = i
		}
		e := b.entries[idx]

		// Apply filters.
		if !opts.Since.IsZero() && e.Time.Before(opts.Since) {
			continue
		}
		if !opts.Before.IsZero() && !e.Time.Before(opts.Before) {
			continue
		}
		if levelToSlog(e.Level) < minLevel {
			continue
		}
		if opts.Component != "" {
			if comp, ok := e.Attrs["component"]; !ok || comp != opts.Component {
				continue
			}
		}

		result = append(result, e)
	}

	// Apply limit (keep the most recent entries).
	if opts.Limit > 0 && len(result) > opts.Limit {
		result = result[len(result)-opts.Limit:]
	}

	return result
}

// levelToSlog converts a level string back to slog.Level for comparison.
func levelToSlog(s string) slog.Level {
	switch s {
	case "DEBUG":
		return slog.LevelDebug
	case "INFO":
		return slog.LevelInfo
	case "WARN":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelDebug
	}
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

// Handle writes the record to the primary handler and captures it in the buffer.
// The slog.Handler interface requires a value receiver for slog.Record.
func (h *TeeHandler) Handle(ctx context.Context, r slog.Record) error { //nolint:gocritic // slog.Handler interface requires value receiver
	// Write to primary handler first.
	err := h.primary.Handle(ctx, r)

	// Capture in ring buffer regardless of primary handler errors.
	entry := LogEntry{
		Time:    r.Time,
		Level:   r.Level.String(),
		Message: r.Message,
	}

	// Collect attributes: handler-level attrs first, then record attrs.
	attrs := make(map[string]any)
	prefix := groupPrefix(h.groups)

	for _, a := range h.attrs {
		attrs[prefix+a.Key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		attrs[prefix+a.Key] = a.Value.Any()
		return true
	})

	if len(attrs) > 0 {
		entry.Attrs = attrs
	}

	h.buf.Add(entry)

	return err
}

// WithAttrs returns a new TeeHandler with the given attributes added.
func (h *TeeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &TeeHandler{
		primary: h.primary.WithAttrs(attrs),
		buf:     h.buf,
		attrs:   append(slices.Clone(h.attrs), attrs...),
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
