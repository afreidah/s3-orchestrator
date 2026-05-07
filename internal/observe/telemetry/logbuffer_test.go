// -------------------------------------------------------------------------------
// Log Buffer Tests
//
// Author: Alex Freidah
//
// Tests for the in-memory ring buffer and TeeHandler. Covers add/retrieve,
// wrapping, filtering by level/time/component, concurrent access, and the
// tee handler writing to both destinations.
// -------------------------------------------------------------------------------

package telemetry

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// -------------------------------------------------------------------------
// LOG BUFFER TESTS
// -------------------------------------------------------------------------

// TestLogBuffer_AddAndRetrieve verifies the log buffer add and retrieve contract.
// Asserts that got entries, want 2.
func TestLogBuffer_AddAndRetrieve(t *testing.T) {
	t.Parallel()
	buf := NewLogBuffer()

	buf.Add(LogEntry{Time: time.Now(), Level: "INFO", Message: "hello"})
	buf.Add(LogEntry{Time: time.Now(), Level: "WARN", Message: "world"})

	entries := buf.Entries(&LogQueryOpts{})
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].Message != "hello" {
		t.Errorf("first entry = %q, want hello", entries[0].Message)
	}
	if entries[1].Message != "world" {
		t.Errorf("second entry = %q, want world", entries[1].Message)
	}
}

// TestLogBuffer_Wraps verifies the log buffer wraps contract.
// Asserts that got entries, want.
func TestLogBuffer_Wraps(t *testing.T) {
	t.Parallel()
	buf := NewLogBuffer()

	// Fill buffer beyond capacity.
	for i := range logBufferCapacity + 100 {
		buf.Add(LogEntry{
			Time:    time.Now(),
			Level:   "INFO",
			Message: "msg",
			Attrs:   map[string]any{"i": i},
		})
	}

	entries := buf.Entries(&LogQueryOpts{})
	if len(entries) != logBufferCapacity {
		t.Fatalf("got %d entries, want %d", len(entries), logBufferCapacity)
	}

	// Oldest entry should be index 100 (first 100 were overwritten).
	first := entries[0].Attrs["i"].(int)
	if first != 100 {
		t.Errorf("oldest entry index = %d, want 100", first)
	}

	last := entries[len(entries)-1].Attrs["i"].(int)
	if last != logBufferCapacity+99 {
		t.Errorf("newest entry index = %d, want %d", last, logBufferCapacity+99)
	}
}

// TestLogBuffer_Empty verifies the log buffer empty contract.
// Asserts that got entries from empty buffer, want nil.
func TestLogBuffer_Empty(t *testing.T) {
	t.Parallel()
	buf := NewLogBuffer()

	entries := buf.Entries(&LogQueryOpts{})
	if entries != nil {
		t.Fatalf("got %d entries from empty buffer, want nil", len(entries))
	}
}

// TestLogBuffer_FilterByLevel verifies the log buffer filter by level contract.
// Asserts that got entries, want 2.
func TestLogBuffer_FilterByLevel(t *testing.T) {
	t.Parallel()
	buf := NewLogBuffer()

	buf.Add(LogEntry{Time: time.Now(), Level: "DEBUG", Message: "debug"})
	buf.Add(LogEntry{Time: time.Now(), Level: "INFO", Message: "info"})
	buf.Add(LogEntry{Time: time.Now(), Level: "WARN", Message: "warn"})
	buf.Add(LogEntry{Time: time.Now(), Level: "ERROR", Message: "error"})

	entries := buf.Entries(&LogQueryOpts{MinLevel: slog.LevelWarn})
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].Message != "warn" {
		t.Errorf("first = %q, want warn", entries[0].Message)
	}
	if entries[1].Message != "error" {
		t.Errorf("second = %q, want error", entries[1].Message)
	}
}

// TestLogBuffer_FilterBySince verifies the log buffer filter by since contract.
// Asserts that got entries, want 1.
func TestLogBuffer_FilterBySince(t *testing.T) {
	t.Parallel()
	buf := NewLogBuffer()

	old := time.Now().Add(-10 * time.Minute)
	recent := time.Now()

	buf.Add(LogEntry{Time: old, Level: "INFO", Message: "old"})
	buf.Add(LogEntry{Time: recent, Level: "INFO", Message: "new"})

	entries := buf.Entries(&LogQueryOpts{Since: time.Now().Add(-1 * time.Minute)})
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Message != "new" {
		t.Errorf("entry = %q, want new", entries[0].Message)
	}
}

// TestLogBuffer_Before verifies the log buffer before contract.
// Asserts that got entries, want 1.
func TestLogBuffer_Before(t *testing.T) {
	t.Parallel()
	buf := NewLogBuffer()

	old := time.Now().Add(-10 * time.Minute)
	mid := time.Now().Add(-5 * time.Minute)
	recent := time.Now()

	buf.Add(LogEntry{Time: old, Level: "INFO", Message: "old"})
	buf.Add(LogEntry{Time: mid, Level: "INFO", Message: "mid"})
	buf.Add(LogEntry{Time: recent, Level: "INFO", Message: "new"})

	// Before mid should return only the old entry.
	entries := buf.Entries(&LogQueryOpts{Before: mid})
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Message != "old" {
		t.Errorf("entry = %q, want old", entries[0].Message)
	}

	// Before recent should return old and mid.
	entries = buf.Entries(&LogQueryOpts{Before: recent})
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].Message != "old" {
		t.Errorf("first = %q, want old", entries[0].Message)
	}
	if entries[1].Message != "mid" {
		t.Errorf("second = %q, want mid", entries[1].Message)
	}

	// Zero Before should return all.
	entries = buf.Entries(&LogQueryOpts{})
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}
}

// TestLogBuffer_FilterByComponent verifies the log buffer filter by component contract.
// Asserts that got entries, want 1.
func TestLogBuffer_FilterByComponent(t *testing.T) {
	t.Parallel()
	buf := NewLogBuffer()

	buf.Add(LogEntry{Time: time.Now(), Level: "INFO", Message: "a", Attrs: map[string]any{"component": "server"}})
	buf.Add(LogEntry{Time: time.Now(), Level: "INFO", Message: "b", Attrs: map[string]any{"component": "storage"}})
	buf.Add(LogEntry{Time: time.Now(), Level: "INFO", Message: "c"})

	entries := buf.Entries(&LogQueryOpts{Component: "server"})
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Message != "a" {
		t.Errorf("entry = %q, want a", entries[0].Message)
	}
}

// TestLogBuffer_Limit verifies the log buffer limit contract.
// Asserts that got entries, want 10.
func TestLogBuffer_Limit(t *testing.T) {
	t.Parallel()
	buf := NewLogBuffer()

	for i := range 100 {
		buf.Add(LogEntry{
			Time:    time.Now(),
			Level:   "INFO",
			Message: "msg",
			Attrs:   map[string]any{"i": i},
		})
	}

	entries := buf.Entries(&LogQueryOpts{Limit: 10})
	if len(entries) != 10 {
		t.Fatalf("got %d entries, want 10", len(entries))
	}

	// Should be the 10 most recent.
	first := entries[0].Attrs["i"].(int)
	if first != 90 {
		t.Errorf("first entry index = %d, want 90", first)
	}
}

// TestLogBuffer_ConcurrentAccess verifies the log buffer concurrent access path by exercising wg.Go, buf.Add, time.Now.
func TestLogBuffer_ConcurrentAccess(t *testing.T) {
	t.Parallel()
	buf := NewLogBuffer()

	var wg sync.WaitGroup
	// Concurrent writers.
	for range 10 {
		wg.Go(func() {
			for range 500 {
				buf.Add(LogEntry{Time: time.Now(), Level: "INFO", Message: "concurrent"})
			}
		})
	}
	// Concurrent readers.
	for range 5 {
		wg.Go(func() {
			for range 100 {
				_ = buf.Entries(&LogQueryOpts{})
			}
		})
	}

	wg.Wait()

	// Just verify no panic or deadlock occurred.
	entries := buf.Entries(&LogQueryOpts{})
	if len(entries) == 0 {
		t.Error("expected entries after concurrent writes")
	}
}

// -------------------------------------------------------------------------
// TEE HANDLER TESTS
// -------------------------------------------------------------------------

// TestTeeHandler_WritesToBoth verifies the tee handler writes to both contract.
// Asserts that buffer has entries, want 1.
func TestTeeHandler_WritesToBoth(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	jsonHandler := slog.NewJSONHandler(&stdout, &slog.HandlerOptions{Level: slog.LevelDebug})
	buf := NewLogBuffer()

	logger := slog.New(NewTeeHandler(jsonHandler, buf))
	logger.InfoContext(context.Background(), "test message", "key", "value")

	// Check stdout got the record.
	if stdout.Len() == 0 {
		t.Error("primary handler received no output")
	}

	// Check buffer got the record.
	entries := buf.Entries(&LogQueryOpts{})
	if len(entries) != 1 {
		t.Fatalf("buffer has %d entries, want 1", len(entries))
	}
	if entries[0].Message != "test message" {
		t.Errorf("message = %q, want 'test message'", entries[0].Message)
	}
	if entries[0].Level != "INFO" {
		t.Errorf("level = %q, want INFO", entries[0].Level)
	}
	if entries[0].Attrs["key"] != "value" {
		t.Errorf("attrs[key] = %v, want 'value'", entries[0].Attrs["key"])
	}
}

// teeBufErr is the kind of error that pre-fix code path stored in the
// ring buffer as a struct, which json.Marshal then rendered as "{}" and
// the UI displayed as "[object Object]". Has no exported fields and no
// MarshalJSON, exactly matching the production failure mode.
type teeBufErr struct{ msg string }

// Error returns the wrapped message.
func (e *teeBufErr) Error() string { return e.msg }

// TestTeeHandler_StringifiesErrorAttr pins the contract that error-typed
// attrs land in the ring buffer as their Error() string rather than as
// the raw error struct, so the UI's /ui/api/logs endpoint emits a
// printable string instead of the empty-object that renders as
// "[object Object]" downstream.
func TestTeeHandler_StringifiesErrorAttr(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	jsonHandler := slog.NewJSONHandler(&stdout, &slog.HandlerOptions{Level: slog.LevelDebug})
	buf := NewLogBuffer()

	logger := slog.New(NewTeeHandler(jsonHandler, buf))
	logger.WarnContext(context.Background(), "HEAD probe failed",
		"backend", "e2", "error", &teeBufErr{msg: "vault token expired"})

	entries := buf.Entries(&LogQueryOpts{})
	if len(entries) != 1 {
		t.Fatalf("buffer has %d entries, want 1", len(entries))
	}
	got, ok := entries[0].Attrs["error"].(string)
	if !ok {
		t.Fatalf("error attr type = %T, want string", entries[0].Attrs["error"])
	}
	if got != "vault token expired" {
		t.Errorf("error attr = %q, want %q", got, "vault token expired")
	}
}

// TestTeeHandler_WithAttrs verifies the tee handler with attrs contract.
// Asserts that buffer has entries, want 1.
func TestTeeHandler_WithAttrs(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	jsonHandler := slog.NewJSONHandler(&stdout, &slog.HandlerOptions{Level: slog.LevelDebug})
	buf := NewLogBuffer()

	logger := slog.New(NewTeeHandler(jsonHandler, buf)).With("component", "test")
	logger.InfoContext(context.Background(), "with attrs")

	entries := buf.Entries(&LogQueryOpts{})
	if len(entries) != 1 {
		t.Fatalf("buffer has %d entries, want 1", len(entries))
	}
	if entries[0].Attrs["component"] != "test" {
		t.Errorf("attrs[component] = %v, want 'test'", entries[0].Attrs["component"])
	}
}

// TestTeeHandler_WithGroup verifies the tee handler with group contract.
// Asserts that buffer has entries, want 1.
func TestTeeHandler_WithGroup(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	jsonHandler := slog.NewJSONHandler(&stdout, &slog.HandlerOptions{Level: slog.LevelDebug})
	buf := NewLogBuffer()

	logger := slog.New(NewTeeHandler(jsonHandler, buf)).WithGroup("db")
	logger.InfoContext(context.Background(), "grouped", "host", "localhost")

	entries := buf.Entries(&LogQueryOpts{})
	if len(entries) != 1 {
		t.Fatalf("buffer has %d entries, want 1", len(entries))
	}
	if entries[0].Attrs["db.host"] != "localhost" {
		t.Errorf("attrs = %v, want db.host=localhost", entries[0].Attrs)
	}
}

// TestTeeHandler_Enabled verifies the tee handler enabled path by exercising slog.NewJSONHandler, handler.Enabled, context.Background.
func TestTeeHandler_Enabled(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	jsonHandler := slog.NewJSONHandler(&stdout, &slog.HandlerOptions{Level: slog.LevelWarn})
	buf := NewLogBuffer()

	handler := NewTeeHandler(jsonHandler, buf)

	if handler.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("should not be enabled for INFO when primary is WARN")
	}
	if !handler.Enabled(context.Background(), slog.LevelWarn) {
		t.Error("should be enabled for WARN")
	}
}

// TestTeeHandler_WithGroupEmpty verifies the tee handler with group empty path by exercising slog.NewJSONHandler, handler.WithGroup.
func TestTeeHandler_WithGroupEmpty(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	jsonHandler := slog.NewJSONHandler(&stdout, &slog.HandlerOptions{Level: slog.LevelDebug})
	buf := NewLogBuffer()

	handler := NewTeeHandler(jsonHandler, buf)
	same := handler.WithGroup("")

	// WithGroup("") should return the same handler.
	if same != handler {
		t.Error("WithGroup(\"\") should return the same handler")
	}
}

// TestLevelToSlog_UnknownLevel verifies the level to slog unknown level contract.
// Asserts that levelToSlog(\"UNKNOWN\") = , want DEBUG.
func TestLevelToSlog_UnknownLevel(t *testing.T) {
	t.Parallel()
	// Unknown level strings should map to DEBUG.
	if got := levelToSlog("UNKNOWN"); got != slog.LevelDebug {
		t.Errorf("levelToSlog(\"UNKNOWN\") = %v, want DEBUG", got)
	}
	if got := levelToSlog(""); got != slog.LevelDebug {
		t.Errorf("levelToSlog(\"\") = %v, want DEBUG", got)
	}
}

// TestTeeHandler_FilterByComponent verifies the tee handler filter by component contract.
// Asserts that got entries, want 1.
func TestTeeHandler_FilterByComponent(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	jsonHandler := slog.NewJSONHandler(&stdout, &slog.HandlerOptions{Level: slog.LevelDebug})
	buf := NewLogBuffer()

	serverLog := slog.New(NewTeeHandler(jsonHandler, buf)).With("component", "server")
	storageLog := slog.New(NewTeeHandler(jsonHandler, buf)).With("component", "storage")

	serverLog.InfoContext(context.Background(), "from server")
	storageLog.InfoContext(context.Background(), "from storage")

	entries := buf.Entries(&LogQueryOpts{Component: "server"})
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Message != "from server" {
		t.Errorf("message = %q, want 'from server'", entries[0].Message)
	}
}
