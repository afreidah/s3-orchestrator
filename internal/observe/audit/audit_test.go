// -------------------------------------------------------------------------------
// Audit Package Tests
//
// Author: Alex Freidah
//
// Validates request ID generation, context propagation, and structured audit log
// output. Ensures audit entries carry the correct request ID and event fields.
// -------------------------------------------------------------------------------

package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"
	"testing"
)

// TestNewID_UniqueAndCorrectLength verifies that generated IDs are 32 hex chars and unique.
func TestNewID_UniqueAndCorrectLength(t *testing.T) {
	ids := make(map[string]bool, 100)
	for range 100 {
		id := NewID()
		if len(id) != 32 { // 16 bytes hex-encoded = 32 chars
			t.Fatalf("expected 32-char ID, got %d: %q", len(id), id)
		}
		if ids[id] {
			t.Fatalf("duplicate ID generated: %q", id)
		}
		ids[id] = true
	}
}

// TestContextRoundTrip verifies request ID storage and retrieval from context.
func TestContextRoundTrip(t *testing.T) {
	ctx := context.Background()

	// Empty context returns empty string
	if got := RequestID(ctx); got != "" {
		t.Fatalf("expected empty string from bare context, got %q", got)
	}

	// Round-trip
	ctx = WithRequestID(ctx, "test-id-123")
	if got := RequestID(ctx); got != "test-id-123" {
		t.Fatalf("expected test-id-123, got %q", got)
	}

	// Override
	ctx = WithRequestID(ctx, "override-456")
	if got := RequestID(ctx); got != "override-456" {
		t.Fatalf("expected override-456, got %q", got)
	}
}

// TestLog_StructuredOutput verifies that audit log entries contain the expected structured fields.
func TestLog_StructuredOutput(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))

	ctx := WithRequestID(context.Background(), "req-abc")
	Log(ctx, "s3.PutObject",
		slog.String("key", "photos/cat.jpg"),
		slog.Int64("size", 12345),
	)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log output: %v\nraw: %s", err, buf.String())
	}

	// Verify required fields
	if entry["audit"] != true {
		t.Errorf("expected audit=true, got %v", entry["audit"])
	}
	if entry["event"] != "s3.PutObject" {
		t.Errorf("expected event=s3.PutObject, got %v", entry["event"])
	}
	if entry["request_id"] != "req-abc" {
		t.Errorf("expected request_id=req-abc, got %v", entry["request_id"])
	}
	if entry["key"] != "photos/cat.jpg" {
		t.Errorf("expected key=photos/cat.jpg, got %v", entry["key"])
	}
	if size, ok := entry["size"].(float64); !ok || int64(size) != 12345 {
		t.Errorf("expected size=12345, got %v", entry["size"])
	}
}

// TestLog_OnEventCallback verifies that the OnEvent callback is invoked with the event name.
func TestLog_OnEventCallback(t *testing.T) {
	var called atomic.Int32
	var lastEvent string
	SetOnEvent(func(event string) {
		called.Add(1)
		lastEvent = event
	})
	defer SetOnEvent(nil)

	Log(context.Background(), "test.event")

	if called.Load() != 1 {
		t.Errorf("expected OnEvent called once, got %d", called.Load())
	}
	if lastEvent != "test.event" {
		t.Errorf("expected event=test.event, got %q", lastEvent)
	}
}

// TestLog_NilOnEvent verifies that Log does not panic when no callback is registered.
func TestLog_NilOnEvent(t *testing.T) {
	SetOnEvent(nil)
	// Should not panic when callback is nil
	Log(context.Background(), "safe.event")
}

// TestSetOnEvent_ConcurrentAccess verifies that SetOnEvent and Log can be
// called concurrently without triggering a data race (run with -race).
func TestSetOnEvent_ConcurrentAccess(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 1000 {
			Log(context.Background(), "concurrent.event")
		}
	}()

	for range 100 {
		SetOnEvent(func(event string) {})
		SetOnEvent(nil)
	}
	<-done
}

// TestSetOnEvent_Replaces verifies that a second SetOnEvent call replaces the
// first callback and that only the active callback is invoked.
func TestSetOnEvent_Replaces(t *testing.T) {
	var first, second atomic.Int32
	SetOnEvent(func(string) { first.Add(1) })
	Log(context.Background(), "e")
	SetOnEvent(func(string) { second.Add(1) })
	Log(context.Background(), "e")
	SetOnEvent(nil)

	if first.Load() != 1 {
		t.Errorf("first callback called %d times, want 1", first.Load())
	}
	if second.Load() != 1 {
		t.Errorf("second callback called %d times, want 1", second.Load())
	}
}

// TestLog_WithoutRequestID verifies that audit entries omit request_id when none is set.
func TestLog_WithoutRequestID(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))

	Log(context.Background(), "rebalance.start")

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log output: %v", err)
	}

	if entry["audit"] != true {
		t.Errorf("expected audit=true")
	}
	if entry["event"] != "rebalance.start" {
		t.Errorf("expected event=rebalance.start, got %v", entry["event"])
	}
	// request_id should not be present
	if _, ok := entry["request_id"]; ok {
		t.Errorf("expected no request_id field, but got %v", entry["request_id"])
	}
}
