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
	"testing"
)

func TestNewID_UniqueAndCorrectLength(t *testing.T) {
	ids := make(map[string]bool, 100)
	for i := 0; i < 100; i++ {
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
