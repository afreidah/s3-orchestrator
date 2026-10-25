// -------------------------------------------------------------------------------
// Admin CLI - status Renderer Tests
//
// Author: Alex Freidah
//
// Covers the bespoke status text renderer: a backends table led by the name,
// humanized byte sizes, the usage limits table, the trailing health and
// instance lines, and the JSON fallback when the body is not the expected
// shape.
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
    {"name":"minio-1","healthy":true,"refused_calls":[],"draining":false,"flags":[],"bytes_used":0,"bytes_limit":10737418240,"object_count":0,"api_requests":0,"ingress_bytes":0,"egress_bytes":0},
    {"name":"minio-2","healthy":false,"refused_calls":["GetObject"],"draining":true,"flags":["cordoned","draining"],"bytes_used":1048576,"bytes_limit":10737418240,"object_count":7,"api_requests":3,"ingress_bytes":2048,"egress_bytes":512,
     "usage_limits":[{"kind":"egress","used":512,"limit":1024},{"kind":"requests","budget":"all","used":3,"limit":1200}],
     "usage_cycle":{"start":"2026-06-15T00:00:00-07:00","next_reset":"2026-07-15T00:00:00-07:00","timezone":"America/Los_Angeles"}},
    {"name":"minio-3","healthy":false,"refused_calls":["PutObject","GetObject","HeadObject","DeleteObject","DeleteObjects","CopyObject","ListObjects"],"draining":false,"flags":[],"bytes_used":0,"bytes_limit":0,"object_count":0,"api_requests":0,"ingress_bytes":0,"egress_bytes":0}
  ],
  "db_healthy": true,
  "usage_period": "2026-06",
  "instance": {"id":"host-1a2b3c4d","version":"v1.2.3","mode":"all","started_at":"2026-06-01T00:00:00Z"}
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
	if code := Command("status", nil, srv.URL, testCreds, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d (stderr=%q)", code, stderr.String())
	}
	out := stdout.String()

	lines := strings.SplitN(out, "\n", 2)
	if !strings.HasPrefix(lines[0], "Backend") {
		t.Errorf("first column header should be Backend, got header line %q", lines[0])
	}
	for _, want := range []string{
		"minio-1", "minio-2", "10.0 GiB", "1.0 MiB",
		"Health", "Refused", "State", "healthy", "degraded", "down", "GetObject", "cordoned,draining",
		"DB healthy:    true",
		"Instance:      host-1a2b3c4d  v1.2.3  mode all",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Usage period") {
		t.Errorf("status output still prints the fleet-wide period:\n%s", out)
	}
	// The backend name must lead its row: minio-2 appears before its byte sizes.
	if i, j := strings.Index(out, "minio-2"), strings.Index(out, "1.0 MiB"); i < 0 || j < 0 || i > j {
		t.Errorf("backend name should lead the row:\n%s", out)
	}
}

// TestStatus_UsageLimitsTable verifies the usage limits print as their own
// table, one row per limit, with units chosen by kind and the share used.
func TestStatus_UsageLimitsTable(t *testing.T) {
	t.Parallel()
	srv := statusServer(t)
	var stdout, stderr bytes.Buffer
	if code := Command("status", nil, srv.URL, testCreds, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d (stderr=%q)", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"Cycle", "Jun 15 - Jul 15 America/Los_Angeles", "Usage limit", "Use%", "egress", "512 B", "1.0 KiB", "50%", "requests:all", "1,200", "0%"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "minio-1   egress") || strings.Count(out, "minio-1") != 1 {
		t.Errorf("an unlimited backend appears in the usage limits table:\n%s", out)
	}
}

// TestStatus_NoUsageLimits verifies a fleet with no limits prints no usage
// limits table.
func TestStatus_NoUsageLimits(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := renderStatus(&buf, []byte(`{"backends":[{"name":"b1"}],"db_healthy":true}`)); err != nil {
		t.Fatalf("renderStatus: %v", err)
	}
	if strings.Contains(buf.String(), "Usage limit") {
		t.Errorf("printed a usage limits table for a fleet with none:\n%s", buf.String())
	}
}

func TestStatus_JSONMode(t *testing.T) {
	t.Parallel()
	srv := statusServer(t)
	var stdout bytes.Buffer
	if code := CommandWithFormat("status", nil, srv.URL, testCreds, output.FormatJSON, &stdout, new(bytes.Buffer)); code != 0 {
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
	if code := Command("status", nil, srv.URL, testCreds, &stdout, new(bytes.Buffer)); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout.String(), "not-an-array") {
		t.Errorf("expected raw-JSON fallback, got:\n%s", stdout.String())
	}
}
