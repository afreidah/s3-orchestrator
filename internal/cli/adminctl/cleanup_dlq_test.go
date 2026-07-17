// -------------------------------------------------------------------------------
// Admin CLI - cleanup-dlq Tests
//
// Author: Alex Freidah
//
// Covers the cleanup-dlq subcommand dispatch and renderers: the list table and
// the requeue summary, including that -backend scopes the request.
// -------------------------------------------------------------------------------

package adminctl

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const cleanupDLQListBody = `{
  "depth": 2,
  "items": [
    {"backend":"b2","object_key":"unified/a.gz","reason":"delete_failed","size_bytes":2048,"attempts":10,"moved_at":"2026-07-07T07:44:51Z","last_error":"backend unavailable"},
    {"backend":"b2","object_key":"unified/b.gz","reason":"delete_failed","size_bytes":1024,"attempts":10,"moved_at":"2026-07-07T07:44:51Z","last_error":"backend unavailable"}
  ]
}`

// TestCleanupDLQ_RenderersRejectBadJSON asserts both renderers surface a
// decode error (which makes the client fall back to raw JSON) on a malformed
// body.
func TestCleanupDLQ_RenderersRejectBadJSON(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := renderCleanupDLQList(&buf, []byte("{not json")); err == nil {
		t.Error("renderCleanupDLQList: expected a decode error")
	}
	if err := renderCleanupDLQRequeue(&buf, []byte("{not json")); err == nil {
		t.Error("renderCleanupDLQRequeue: expected a decode error")
	}
}

// TestCleanupDLQ_ListRendersTable asserts `cleanup-dlq list -backend` hits the
// scoped endpoint and renders the row table plus the DLQ depth.
func TestCleanupDLQ_ListRendersTable(t *testing.T) {
	t.Parallel()
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		_, _ = w.Write([]byte(cleanupDLQListBody))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	if code := Command("cleanup-dlq", []string{"list", "-backend", "b2"}, srv.URL, "tok", &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d (stderr=%q)", code, stderr.String())
	}
	if gotPath != "/admin/api/cleanup-dlq" {
		t.Errorf("path = %q, want /admin/api/cleanup-dlq", gotPath)
	}
	if !strings.Contains(gotQuery, "backend=b2") {
		t.Errorf("query %q missing backend=b2", gotQuery)
	}
	out := stdout.String()
	for _, want := range []string{"Backend", "b2", "unified/a.gz", "backend unavailable", "2.0 KiB", "DLQ depth: 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
}

// TestCleanupDLQ_RequeueReportsCount asserts `cleanup-dlq requeue -backend`
// POSTs to the scoped requeue endpoint and reports the moved-row count.
func TestCleanupDLQ_RequeueReportsCount(t *testing.T) {
	t.Parallel()
	var gotMethod, gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		_, _ = w.Write([]byte(`{"backend":"b2","requeued":106}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	if code := Command("cleanup-dlq", []string{"requeue", "-backend", "b2"}, srv.URL, "tok", &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d (stderr=%q)", code, stderr.String())
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/admin/api/cleanup-dlq/requeue" {
		t.Errorf("path = %q, want /admin/api/cleanup-dlq/requeue", gotPath)
	}
	if !strings.Contains(gotQuery, "backend=b2") {
		t.Errorf("query %q missing backend=b2", gotQuery)
	}
	if out := stdout.String(); !strings.Contains(out, "Requeued 106") || !strings.Contains(out, "backend b2") {
		t.Errorf("requeue output unexpected:\n%s", out)
	}
}
