// -------------------------------------------------------------------------------
// Admin CLI Tests
//
// Author: Alex Freidah
//
// Tests for Command covering drain, drain-status, drain-cancel, remove-backend,
// and edge cases (missing arguments, unknown commands, --purge flag).
// -------------------------------------------------------------------------------

package adminctl

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCommand_Drain_MissingBackend(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Command("drain", nil, "http://unused", "tok", &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "backend name is required") {
		t.Errorf("stderr = %q, want 'backend name is required'", stderr.String())
	}
}

func TestCommand_DrainStatus_MissingBackend(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Command("drain-status", nil, "http://unused", "tok", &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "backend name is required") {
		t.Errorf("stderr = %q, want 'backend name is required'", stderr.String())
	}
}

func TestCommand_DrainCancel_MissingBackend(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Command("drain-cancel", nil, "http://unused", "tok", &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "backend name is required") {
		t.Errorf("stderr = %q, want 'backend name is required'", stderr.String())
	}
}

func TestCommand_RemoveBackend_MissingBackend(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Command("remove-backend", nil, "http://unused", "tok", &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "backend name is required") {
		t.Errorf("stderr = %q, want 'backend name is required'", stderr.String())
	}
}

func TestCommand_Unknown(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Command("nonexistent", nil, "http://unused", "tok", &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "unknown admin command") {
		t.Errorf("stderr = %q, want 'unknown admin command'", stderr.String())
	}
}

func TestCommand_Drain_SendsPost(t *testing.T) {
	var gotMethod, gotPath, gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotToken = r.Header.Get("X-Admin-Token")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "drain started"})
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Command("drain", []string{"mybackend"}, srv.URL, "secret", &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/admin/api/backends/mybackend/drain" {
		t.Errorf("path = %q, want /admin/api/backends/mybackend/drain", gotPath)
	}
	if gotToken != "secret" {
		t.Errorf("token = %q, want secret", gotToken)
	}
}

func TestCommand_DrainStatus_SendsGet(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{"active": false})
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Command("drain-status", []string{"oci"}, srv.URL, "tok", &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotPath != "/admin/api/backends/oci/drain" {
		t.Errorf("path = %q, want /admin/api/backends/oci/drain", gotPath)
	}
}

func TestCommand_DrainCancel_SendsDelete(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "drain cancelled"})
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Command("drain-cancel", []string{"oci"}, srv.URL, "tok", &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	if gotPath != "/admin/api/backends/oci/drain" {
		t.Errorf("path = %q, want /admin/api/backends/oci/drain", gotPath)
	}
}

func TestCommand_RemoveBackend_SendsDelete(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "backend removed"})
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Command("remove-backend", []string{"oci"}, srv.URL, "tok", &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	if gotPath != "/admin/api/backends/oci" {
		t.Errorf("path = %q, want /admin/api/backends/oci", gotPath)
	}
	if gotQuery != "" {
		t.Errorf("query = %q, want empty", gotQuery)
	}
}

func TestCommand_RemoveBackend_Purge(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "backend removed"})
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Command("remove-backend", []string{"-purge", "oci"}, srv.URL, "tok", &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if gotPath != "/admin/api/backends/oci" {
		t.Errorf("path = %q, want /admin/api/backends/oci", gotPath)
	}
	if gotQuery != "purge=true" {
		t.Errorf("query = %q, want purge=true", gotQuery)
	}
}

func TestCommand_Reconcile_DefaultPostsAllBackends(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Command("reconcile", nil, srv.URL, "tok", &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/admin/api/reconcile" {
		t.Errorf("path = %q, want /admin/api/reconcile", gotPath)
	}
	if gotQuery != "" {
		t.Errorf("query = %q, want empty", gotQuery)
	}
}

func TestCommand_Reconcile_ScopesToBackend(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Command("reconcile", []string{"-backend", "g3"}, srv.URL, "tok", &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if gotQuery != "backend=g3" {
		t.Errorf("query = %q, want backend=g3", gotQuery)
	}
}

func TestCommand_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "backend not found"})
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Command("drain", []string{"bad"}, srv.URL, "tok", &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "backend not found") {
		t.Errorf("stdout = %q, want to contain 'backend not found'", stdout.String())
	}
}

// jsonOK echoes a deterministic JSON envelope so doRequest's pretty-printer
// finishes with non-zero output. Tests can ignore the body and only assert
// the request shape.
func jsonOK(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// TestCommand_SimpleGetAndPostWrappers walks every handler that is a
// one-line wrapper around doGet/doPost so the matrix is covered without
// duplicating boilerplate per command.
func TestCommand_SimpleGetAndPostWrappers(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantMethod  string
		wantPath    string
		wantQueryIn string // substring match (some commands optionally append ?...)
	}{
		{"status", nil, http.MethodGet, "/admin/api/status", ""},
		{"cleanup-queue", nil, http.MethodGet, "/admin/api/cleanup-queue", ""},
		{"usage-flush", nil, http.MethodPost, "/admin/api/usage-flush", ""},
		{"replicate", nil, http.MethodPost, "/admin/api/replicate", ""},
		{"over-replication", nil, http.MethodGet, "/admin/api/over-replication", ""},
		{"over-replication-execute", []string{"-execute"}, http.MethodPost, "/admin/api/over-replication", ""},
		{"over-replication-execute-batch", []string{"-execute", "-batch-size", "200"}, http.MethodPost, "/admin/api/over-replication", "batch_size=200"},
		{"log-level-get", nil, http.MethodGet, "/admin/api/log-level", ""},
		{"log-level-set", []string{"-set", "debug"}, http.MethodPut, "/admin/api/log-level", ""},
		{"scrub", nil, http.MethodPost, "/admin/api/scrub", ""},
		{"scrub-batch", []string{"-batch-size", "50"}, http.MethodPost, "/admin/api/scrub", "batch_size=50"},
		{"backfill-checksums", nil, http.MethodPost, "/admin/api/backfill-checksums", ""},
		{"backfill-checksums-batch", []string{"-batch-size", "50"}, http.MethodPost, "/admin/api/backfill-checksums", "batch_size=50"},
		{"object-locations", []string{"-key", "my/key"}, http.MethodGet, "/admin/api/object-locations", "key=my/key"},
	}

	cmdName := func(name string) string {
		// Most cases reuse the subcommand's actual name; the variants encode
		// the flag combination in the case name, so strip the suffix.
		switch name {
		case "over-replication-execute", "over-replication-execute-batch":
			return "over-replication"
		case "log-level-get", "log-level-set":
			return "log-level"
		case "scrub-batch":
			return "scrub"
		case "backfill-checksums-batch":
			return "backfill-checksums"
		}
		return name
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotMethod, gotPath, gotQuery string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
				jsonOK(w, r)
			}))
			defer srv.Close()

			var stdout, stderr bytes.Buffer
			code := Command(cmdName(tc.name), tc.args, srv.URL, "tok", &stdout, &stderr)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0 (stderr=%q)", code, stderr.String())
			}
			if gotMethod != tc.wantMethod {
				t.Errorf("method = %q, want %q", gotMethod, tc.wantMethod)
			}
			if gotPath != tc.wantPath {
				t.Errorf("path = %q, want %q", gotPath, tc.wantPath)
			}
			if tc.wantQueryIn != "" && !strings.Contains(gotQuery, tc.wantQueryIn) {
				t.Errorf("query = %q, want to contain %q", gotQuery, tc.wantQueryIn)
			}
		})
	}
}

// TestCommand_ObjectLocations_MissingKey covers the early-exit branch of
// cmdObjectLocations where the required -key flag is not provided.
func TestCommand_ObjectLocations_MissingKey(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Command("object-locations", nil, "http://unused", "tok", &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "-key is required") {
		t.Errorf("stderr = %q, want -key required message", stderr.String())
	}
}

// TestCommand_RemoveBackend_PurgePreviewPrintsCount drives the preview
// branch of remove-backend (--purge without --confirm) so doRemovePreview's
// formatted output runs.
func TestCommand_RemoveBackend_PurgePreviewPrintsCount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object_count": 42.0,
			"total_bytes":  1024.0,
		})
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Command("remove-backend", []string{"-purge", "oci"}, srv.URL, "tok", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "42 objects") {
		t.Errorf("stdout = %q, want to mention 42 objects", stdout.String())
	}
}

// TestCommand_RemoveBackend_PurgeConfirm covers the two-phase confirm flow:
// preview returns a confirm_token, the second DELETE includes it.
func TestCommand_RemoveBackend_PurgeConfirm(t *testing.T) {
	var deleteCount int
	var lastQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deleteCount++
		lastQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{
			"confirm_token": "abc123",
			"object_count":  1.0,
		})
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Command("remove-backend", []string{"-purge", "-confirm", "oci"}, srv.URL, "tok", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", code, stderr.String())
	}
	if deleteCount != 2 {
		t.Errorf("got %d DELETE calls, want 2", deleteCount)
	}
	if !strings.Contains(lastQuery, "confirm=abc123") {
		t.Errorf("last query = %q, want to contain confirm=abc123", lastQuery)
	}
}

// TestCommand_RemoveBackend_PurgeMissingToken covers doRemovePurge's error
// path when the server fails to return a confirmation token.
func TestCommand_RemoveBackend_PurgeMissingToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Command("remove-backend", []string{"-purge", "-confirm", "oci"}, srv.URL, "tok", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "confirmation token") {
		t.Errorf("stderr = %q, want to mention confirmation token", stderr.String())
	}
}

// TestCommand_TransportError covers the connection-failure branch of
// doRequest by pointing at a closed listener.
func TestCommand_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(jsonOK))
	srv.Close() // immediately close so connections fail

	var stdout, stderr bytes.Buffer
	code := Command("status", nil, srv.URL, "tok", &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "error") {
		t.Errorf("stderr = %q, want to contain an error", stderr.String())
	}
}