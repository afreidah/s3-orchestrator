// -------------------------------------------------------------------------------
// Trace Handler Tests
//
// Author: Alex Freidah
//
// Tests for the TraceHandler slog wrapper that injects OpenTelemetry trace
// context into log records.
// -------------------------------------------------------------------------------

package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

// -------------------------------------------------------------------------
// HANDLE
// -------------------------------------------------------------------------

func TestTraceHandler_NoSpan(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	handler := NewTraceHandler(inner)
	logger := slog.New(handler)

	logger.InfoContext(context.Background(), "no span")

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if _, ok := entry["trace_id"]; ok {
		t.Error("expected no trace_id without active span")
	}
	if _, ok := entry["span_id"]; ok {
		t.Error("expected no span_id without active span")
	}
}

func TestTraceHandler_WithSpan(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	handler := NewTraceHandler(inner)
	logger := slog.New(handler)

	// Create a valid span context with known IDs.
	traceID := trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	spanID := trace.SpanID{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	logger.InfoContext(ctx, "with span")

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got, ok := entry["trace_id"].(string); !ok || got != "0102030405060708090a0b0c0d0e0f10" {
		t.Errorf("trace_id = %q, want %q", got, "0102030405060708090a0b0c0d0e0f10")
	}
	if got, ok := entry["span_id"].(string); !ok || got != "1112131415161718" {
		t.Errorf("span_id = %q, want %q", got, "1112131415161718")
	}
}

// -------------------------------------------------------------------------
// WITH ATTRS / WITH GROUP
// -------------------------------------------------------------------------

func TestTraceHandler_WithAttrs(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	handler := NewTraceHandler(inner)
	logger := slog.New(handler).With("component", "test")

	traceID := trace.TraceID{0xaa, 0xbb, 0xcc, 0xdd, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01}
	spanID := trace.SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	logger.InfoContext(ctx, "attrs test")

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if entry["component"] != "test" {
		t.Errorf("component = %v, want %q", entry["component"], "test")
	}
	if _, ok := entry["trace_id"].(string); !ok {
		t.Error("expected trace_id with active span")
	}
}

func TestTraceHandler_WithGroup(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	handler := NewTraceHandler(inner)
	logger := slog.New(handler).WithGroup("db")

	logger.InfoContext(context.Background(), "group test", "query", "SELECT 1")

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	dbGroup, ok := entry["db"].(map[string]any)
	if !ok {
		t.Fatal("expected 'db' group in output")
	}
	if dbGroup["query"] != "SELECT 1" {
		t.Errorf("db.query = %v, want %q", dbGroup["query"], "SELECT 1")
	}
}

// -------------------------------------------------------------------------
// ENABLED
// -------------------------------------------------------------------------

func TestTraceHandler_Enabled(t *testing.T) {
	t.Parallel()
	inner := slog.NewJSONHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelWarn})
	handler := NewTraceHandler(inner)

	if handler.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("expected INFO to be disabled when min level is WARN")
	}
	if !handler.Enabled(context.Background(), slog.LevelError) {
		t.Error("expected ERROR to be enabled when min level is WARN")
	}
}
