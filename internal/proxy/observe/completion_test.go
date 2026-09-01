// -------------------------------------------------------------------------------
// Per-Operation Completion Helpers - Tests
//
// Author: Alex Freidah
//
// Smoke tests that pin the audit-event name + audit-attribute keys and the
// notification-event type + data-key set for each helper. The whole point
// of this package is that these shapes do not drift across the call sites
// in object/, multipart/, and writepath/, so any rename or attribute drop
// here should fail loudly.
// -------------------------------------------------------------------------------

package observe

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/trace/noop"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
)

// captureAudit swaps the default slog handler for a JSON buffer for the
// duration of fn, then parses the captured line. Mirrors the pattern in
// internal/observe/audit/audit_test.go.
func captureAudit(t *testing.T, fn func()) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(prev)

	fn()

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse audit JSON: %v\nraw: %s", err, buf.String())
	}
	return entry
}

// captureEvent registers an emitter for the duration of fn and returns the
// emitted event (or nil if none was emitted).
func captureEvent(t *testing.T, fn func()) *event.Event {
	t.Helper()
	var got *event.Event
	event.SetEmitter(func(e event.Event) { got = &e })
	defer event.SetEmitter(nil)
	fn()
	return got
}

// reqCtx returns a context with a deterministic request_id so the audit
// + event payloads include the field consistently.
func reqCtx() context.Context {
	return audit.WithRequestID(context.Background(), "req-test")
}

// -------------------------------------------------------------------------
// WRITE-PATH
// -------------------------------------------------------------------------

func TestPutCompleted(t *testing.T) {
	_, sp := noop.NewTracerProvider().Tracer("t").Start(context.Background(), "s")
	ctx := reqCtx()

	var emitted *event.Event
	event.SetEmitter(func(e event.Event) { emitted = &e })
	defer event.SetEmitter(nil)

	entry := captureAudit(t, func() {
		PutCompleted(ctx, sp, "bucket/photos/cat.jpg", "minio-1", 12345)
	})

	if entry["event"] != "storage.PutObject" {
		t.Errorf("audit event = %v, want storage.PutObject", entry["event"])
	}
	if entry["key"] != "bucket/photos/cat.jpg" || entry["backend"] != "minio-1" {
		t.Errorf("audit attrs unexpected: %v", entry)
	}
	if emitted == nil || emitted.Type != event.ObjectCreatedPut {
		t.Fatalf("event = %+v, want ObjectCreatedPut", emitted)
	}
	for _, k := range []string{"bucket", "key", "backend", "size", "request_id"} {
		if _, ok := emitted.Data[k]; !ok {
			t.Errorf("event missing data key %q: %+v", k, emitted.Data)
		}
	}
}

func TestCopyCompleted(t *testing.T) {
	_, sp := noop.NewTracerProvider().Tracer("t").Start(context.Background(), "s")
	ctx := reqCtx()

	ev := captureEvent(t, func() {
		entry := captureAudit(t, func() {
			CopyCompleted(ctx, sp, "src/k", "bucket/dst", "minio-1", "minio-2", 99)
		})
		if entry["event"] != "storage.CopyObject" {
			t.Errorf("audit event = %v", entry["event"])
		}
		for _, k := range []string{"source_key", "dest_key", "src_backend", "dest_backend", "size"} {
			if _, ok := entry[k]; !ok {
				t.Errorf("audit missing attr %q: %v", k, entry)
			}
		}
	})
	if ev == nil || ev.Type != event.ObjectCreatedCopy {
		t.Fatalf("event = %+v, want ObjectCreatedCopy", ev)
	}
	if _, ok := ev.Data["source_key"]; !ok {
		t.Errorf("event missing source_key: %+v", ev.Data)
	}
}

func TestDeleteCompleted(t *testing.T) {
	_, sp := noop.NewTracerProvider().Tracer("t").Start(context.Background(), "s")
	ctx := reqCtx()

	ev := captureEvent(t, func() {
		entry := captureAudit(t, func() {
			DeleteCompleted(ctx, sp, "bucket/k", 3)
		})
		if entry["event"] != "storage.DeleteObject" {
			t.Errorf("audit event = %v", entry["event"])
		}
		if entry["copies_deleted"].(float64) != 3 {
			t.Errorf("audit copies_deleted = %v, want 3", entry["copies_deleted"])
		}
	})
	if ev == nil || ev.Type != event.ObjectRemovedDelete {
		t.Fatalf("event = %+v, want ObjectRemovedDelete", ev)
	}
}

func TestDeleteBatchCompleted(t *testing.T) {
	_, sp := noop.NewTracerProvider().Tracer("t").Start(context.Background(), "s")
	ctx := reqCtx()

	ev := captureEvent(t, func() {
		entry := captureAudit(t, func() {
			DeleteBatchCompleted(ctx, sp, 10, 8, 2)
		})
		if entry["event"] != "storage.DeleteObjects" {
			t.Errorf("audit event = %v", entry["event"])
		}
		if entry["deleted"].(float64) != 8 || entry["errors"].(float64) != 2 {
			t.Errorf("audit counts unexpected: %v", entry)
		}
	})
	if ev == nil || ev.Type != event.ObjectRemovedDeleteBatch {
		t.Fatalf("event = %+v, want ObjectRemovedDeleteBatch", ev)
	}
}

// -------------------------------------------------------------------------
// READ-PATH (no event.Emit, no span status)
// -------------------------------------------------------------------------

func TestGetCompleted(t *testing.T) {
	ctx := reqCtx()
	ev := captureEvent(t, func() {
		entry := captureAudit(t, func() {
			GetCompleted(ctx, "bucket/k", "minio-1", 42)
		})
		if entry["event"] != "storage.GetObject" {
			t.Errorf("audit event = %v", entry["event"])
		}
	})
	if ev != nil {
		t.Errorf("Get should not emit notification event, got %+v", ev)
	}
}

func TestHeadCompleted(t *testing.T) {
	ctx := reqCtx()
	entry := captureAudit(t, func() {
		HeadCompleted(ctx, "bucket/k", "minio-1", 42)
	})
	if entry["event"] != "storage.HeadObject" {
		t.Errorf("audit event = %v", entry["event"])
	}
}

func TestListCompleted(t *testing.T) {
	ctx := reqCtx()
	entry := captureAudit(t, func() {
		ListCompleted(ctx, "photos/", 7, true)
	})
	if entry["event"] != "storage.ListObjects" {
		t.Errorf("audit event = %v", entry["event"])
	}
	if entry["truncated"] != true {
		t.Errorf("audit truncated = %v, want true", entry["truncated"])
	}
}

// -------------------------------------------------------------------------
// MULTIPART
// -------------------------------------------------------------------------

func TestMultipartCreated(t *testing.T) {
	_, sp := noop.NewTracerProvider().Tracer("t").Start(context.Background(), "s")
	ctx := reqCtx()

	ev := captureEvent(t, func() {
		entry := captureAudit(t, func() {
			MultipartCreated(ctx, sp, "bucket/k", "minio-1", "upl-abc")
		})
		if entry["event"] != "storage.CreateMultipartUpload" {
			t.Errorf("audit event = %v", entry["event"])
		}
		if entry["upload_id"] != "upl-abc" {
			t.Errorf("audit upload_id = %v", entry["upload_id"])
		}
	})
	if ev != nil {
		t.Errorf("CreateMultipartUpload should not emit notification, got %+v", ev)
	}
}

func TestUploadPartCompleted(t *testing.T) {
	_, sp := noop.NewTracerProvider().Tracer("t").Start(context.Background(), "s")
	ctx := reqCtx()

	ev := captureEvent(t, func() {
		entry := captureAudit(t, func() {
			UploadPartCompleted(ctx, sp, "bucket/k", "minio-1", "upl-abc", 3, 1024)
		})
		if entry["event"] != "storage.UploadPart" {
			t.Errorf("audit event = %v", entry["event"])
		}
		if entry["part_number"].(float64) != 3 {
			t.Errorf("audit part_number = %v", entry["part_number"])
		}
	})
	if ev != nil {
		t.Errorf("UploadPart should not emit notification (S3 parity), got %+v", ev)
	}
}

func TestMultipartAborted(t *testing.T) {
	_, sp := noop.NewTracerProvider().Tracer("t").Start(context.Background(), "s")
	ctx := reqCtx()

	ev := captureEvent(t, func() {
		entry := captureAudit(t, func() {
			MultipartAborted(ctx, sp, "upl-abc", "bucket/k", "minio-1", 5)
		})
		if entry["event"] != "storage.AbortMultipartUpload" {
			t.Errorf("audit event = %v", entry["event"])
		}
		if entry["parts_cleaned"].(float64) != 5 {
			t.Errorf("audit parts_cleaned = %v", entry["parts_cleaned"])
		}
	})
	if ev != nil {
		t.Errorf("AbortMultipartUpload should not emit notification, got %+v", ev)
	}
}

func TestMultipartCompleted(t *testing.T) {
	_, sp := noop.NewTracerProvider().Tracer("t").Start(context.Background(), "s")
	ctx := reqCtx()

	ev := captureEvent(t, func() {
		entry := captureAudit(t, func() {
			MultipartCompleted(ctx, sp, "bucket/k", "minio-1", "upl-abc", 100000, 4)
		})
		if entry["event"] != "storage.CompleteMultipartUpload" {
			t.Errorf("audit event = %v", entry["event"])
		}
		if entry["parts_count"].(float64) != 4 {
			t.Errorf("audit parts_count = %v", entry["parts_count"])
		}
	})
	if ev == nil || ev.Type != event.ObjectCreatedCompleteMultipartUpload {
		t.Fatalf("event = %+v, want ObjectCreatedCompleteMultipartUpload", ev)
	}
	if _, ok := ev.Data["parts_count"]; !ok {
		t.Errorf("event missing parts_count: %+v", ev.Data)
	}
}
