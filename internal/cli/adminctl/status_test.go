// -------------------------------------------------------------------------------
// Admin CLI - status Renderer Tests
//
// Author: Alex Freidah
//
// Covers the bespoke status text renderer: a backends table led by the name,
// humanized byte sizes, the trailing health/period block, and the JSON fallback
// when the body is not the expected shape.
// -------------------------------------------------------------------------------

package adminctl

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/cli/output"
)

const statusBody = `{
  "backends": [
    {"name":"minio-1","healthy":true,"draining":false,"bytes_used":0,"bytes_limit":10737418240,"object_count":0,"api_requests":0,"ingress_bytes":0,"egress_bytes":0},
    {"name":"minio-2","healthy":false,"draining":true,"bytes_used":1048576,"bytes_limit":10737418240,"object_count":7,"api_requests":3,"ingress_bytes":2048,"egress_bytes":512}
  ],
  "db_healthy": true,
  "usage_period": "2026-06"
}`

func statusServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(statusBody))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestStatus_TextTable(t *testing.T) {
	t.Parallel()
	srv := statusServer(t)
	var stdout, stderr bytes.Buffer
	if code := Command("status", nil, srv.URL, "tok", &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d (stderr=%q)", code, stderr.String())
	}
	out := stdout.String()

	lines := strings.SplitN(out, "\n", 2)
	if !strings.HasPrefix(lines[0], "Backend") {
		t.Errorf("first column header should be Backend, got header line %q", lines[0])
	}
	for _, want := range []string{
		"minio-1", "minio-2", "10.0 GiB", "1.0 MiB",
		"Health", "Drain", "healthy", "unhealthy", "draining",
		"DB healthy:    true", "Usage period:  2026-06",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
	// The backend name must lead its row: minio-2 appears before its byte sizes.
	if i, j := strings.Index(out, "minio-2"), strings.Index(out, "1.0 MiB"); i < 0 || j < 0 || i > j {
		t.Errorf("backend name should lead the row:\n%s", out)
	}
}

func TestStatus_JSONMode(t *testing.T) {
	t.Parallel()
	srv := statusServer(t)
	var stdout bytes.Buffer
	if code := CommandWithFormat("status", nil, srv.URL, "tok", output.FormatJSON, &stdout, new(bytes.Buffer)); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	// JSON mode preserves raw bytes (no humanizing, keys intact).
	if !strings.Contains(stdout.String(), `"bytes_limit": 10737418240`) {
		t.Errorf("json mode should keep raw bytes:\n%s", stdout.String())
	}
}

func TestStatus_FallsBackOnUnexpectedShape(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"backends":"not-an-array"}`))
	}))
	defer srv.Close()
	var stdout bytes.Buffer
	// renderStatus errors on the bad shape; the client falls back to raw JSON.
	if code := Command("status", nil, srv.URL, "tok", &stdout, new(bytes.Buffer)); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout.String(), "not-an-array") {
		t.Errorf("expected raw-JSON fallback, got:\n%s", stdout.String())
	}
}
