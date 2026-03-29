// -------------------------------------------------------------------------------
// Tracing Tests - OpenTelemetry Setup and Helpers
//
// Author: Alex Freidah
//
// Tests for tracer initialization, span creation helpers, and common attribute
// builders. Validates disabled config returns no-op, and helper functions
// produce correct attributes.
// -------------------------------------------------------------------------------

package telemetry

import (
	"context"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// -------------------------------------------------------------------------
// InitTracer
// -------------------------------------------------------------------------

func TestInitTracer_Disabled(t *testing.T) {
	cfg := config.TracingConfig{
		Enabled: false,
	}

	shutdown, err := InitTracer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("InitTracer(disabled): %v", err)
	}
	if shutdown == nil {
		t.Fatal("expected non-nil shutdown function")
	}

	// Calling shutdown should be a no-op
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// -------------------------------------------------------------------------
// Tracer
// -------------------------------------------------------------------------

func TestTracer_ReturnsNonNil(t *testing.T) {
	tracer := Tracer()
	if tracer == nil {
		t.Fatal("expected non-nil tracer")
	}
}

// -------------------------------------------------------------------------
// StartSpan
// -------------------------------------------------------------------------

func TestStartSpan_ReturnsContextAndSpan(t *testing.T) {
	ctx, span := StartSpan(context.Background(), "test-span")
	if ctx == nil {
		t.Fatal("expected non-nil context")
	}
	if span == nil {
		t.Fatal("expected non-nil span")
	}
	span.End()
}

func TestStartSpan_WithAttributes(t *testing.T) {
	ctx, span := StartSpan(context.Background(), "test-span",
		AttrObjectKey.String("test-key"),
		AttrBackendName.String("test-backend"),
	)
	if ctx == nil {
		t.Fatal("expected non-nil context")
	}
	if span == nil {
		t.Fatal("expected non-nil span")
	}
	span.End()
}

// -------------------------------------------------------------------------
// RequestAttributes
// -------------------------------------------------------------------------

func TestRequestAttributes_ReturnsCorrectCount(t *testing.T) {
	attrs := RequestAttributes("GET", "/bucket/key", "mybucket", "mykey", "192.168.1.1")
	if len(attrs) != 5 {
		t.Errorf("expected 5 attributes, got %d", len(attrs))
	}
}

// -------------------------------------------------------------------------
// BackendAttributes
// -------------------------------------------------------------------------

func TestBackendAttributes_ReturnsCorrectCount(t *testing.T) {
	attrs := BackendAttributes("PutObject", "b1", "https://s3.example.com", "bucket", "key")
	if len(attrs) != 5 {
		t.Errorf("expected 5 attributes, got %d", len(attrs))
	}
}
