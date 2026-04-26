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