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

func TestLogBuffer_AddAndRetrieve(t *testing.T) {
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

func TestLogBuffer_Wraps(t *testing.T) {
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

func TestLogBuffer_Empty(t *testing.T) {
	buf := NewLogBuffer()

	entries := buf.Entries(&LogQueryOpts{})
	if entries != nil {
		t.Fatalf("got %d entries from empty buffer, want nil", len(entries))
	}
}

func TestLogBuffer_FilterByLevel(t *testing.T) {
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

func TestLogBuffer_FilterBySince(t *testing.T) {
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

func TestLogBuffer_Before(t *testing.T) {
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

func TestLogBuffer_FilterByComponent(t *testing.T) {
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

func TestLogBuffer_Limit(t *testing.T) {
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

func TestLogBuffer_ConcurrentAccess(t *testing.T) {
	buf := NewLogBuffer()

	var wg sync.WaitGroup
	// Concurrent writers.
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 500 {
				buf.Add(LogEntry{Time: time.Now(), Level: "INFO", Message: "concurrent"})
			}
		}()
	}
	// Concurrent readers.
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				_ = buf.Entries(&LogQueryOpts{})
			}
		}()
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

func TestTeeHandler_WritesToBoth(t *testing.T) {
	var stdout bytes.Buffer
	jsonHandler := slog.NewJSONHandler(&stdout, &slog.HandlerOptions{Level: slog.LevelDebug})
	buf := NewLogBuffer()

	logger := slog.New(NewTeeHandler(jsonHandler, buf))
	logger.Info("test message", "key", "value")

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

func TestTeeHandler_WithAttrs(t *testing.T) {
	var stdout bytes.Buffer
	jsonHandler := slog.NewJSONHandler(&stdout, &slog.HandlerOptions{Level: slog.LevelDebug})
	buf := NewLogBuffer()

	logger := slog.New(NewTeeHandler(jsonHandler, buf)).With("component", "test")
	logger.Info("with attrs")

	entries := buf.Entries(&LogQueryOpts{})
	if len(entries) != 1 {
		t.Fatalf("buffer has %d entries, want 1", len(entries))
	}
	if entries[0].Attrs["component"] != "test" {
		t.Errorf("attrs[component] = %v, want 'test'", entries[0].Attrs["component"])
	}
}

func TestTeeHandler_WithGroup(t *testing.T) {
	var stdout bytes.Buffer
	jsonHandler := slog.NewJSONHandler(&stdout, &slog.HandlerOptions{Level: slog.LevelDebug})
	buf := NewLogBuffer()

	logger := slog.New(NewTeeHandler(jsonHandler, buf)).WithGroup("db")
	logger.Info("grouped", "host", "localhost")

	entries := buf.Entries(&LogQueryOpts{})
	if len(entries) != 1 {
		t.Fatalf("buffer has %d entries, want 1", len(entries))
	}
	if entries[0].Attrs["db.host"] != "localhost" {
		t.Errorf("attrs = %v, want db.host=localhost", entries[0].Attrs)
	}
}

func TestTeeHandler_Enabled(t *testing.T) {
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

func TestTeeHandler_WithGroupEmpty(t *testing.T) {
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

func TestLevelToSlog_UnknownLevel(t *testing.T) {
	// Unknown level strings should map to DEBUG.
	if got := levelToSlog("UNKNOWN"); got != slog.LevelDebug {
		t.Errorf("levelToSlog(\"UNKNOWN\") = %v, want DEBUG", got)
	}
	if got := levelToSlog(""); got != slog.LevelDebug {
		t.Errorf("levelToSlog(\"\") = %v, want DEBUG", got)
	}
}

func TestTeeHandler_FilterByComponent(t *testing.T) {
	var stdout bytes.Buffer
	jsonHandler := slog.NewJSONHandler(&stdout, &slog.HandlerOptions{Level: slog.LevelDebug})
	buf := NewLogBuffer()

	serverLog := slog.New(NewTeeHandler(jsonHandler, buf)).With("component", "server")
	storageLog := slog.New(NewTeeHandler(jsonHandler, buf)).With("component", "storage")

	serverLog.Info("from server")
	storageLog.Info("from storage")

	entries := buf.Entries(&LogQueryOpts{Component: "server"})
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Message != "from server" {
		t.Errorf("message = %q, want 'from server'", entries[0].Message)
	}
}
