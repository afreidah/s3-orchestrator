// -------------------------------------------------------------------------------
// Admin Subcommand Tests
//
// Author: Alex Freidah
//
// Tests for the adminCommand function covering drain, drain-status, drain-cancel,
// remove-backend, and edge cases (missing arguments, unknown commands, --purge flag).
// -------------------------------------------------------------------------------

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminCommand_Drain_MissingBackend(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := adminCommand("drain", nil, "http://unused", "tok", &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "backend name is required") {
		t.Errorf("stderr = %q, want 'backend name is required'", stderr.String())
	}
}

func TestAdminCommand_DrainStatus_MissingBackend(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := adminCommand("drain-status", nil, "http://unused", "tok", &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "backend name is required") {
		t.Errorf("stderr = %q, want 'backend name is required'", stderr.String())
	}
}

func TestAdminCommand_DrainCancel_MissingBackend(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := adminCommand("drain-cancel", nil, "http://unused", "tok", &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "backend name is required") {
		t.Errorf("stderr = %q, want 'backend name is required'", stderr.String())
	}
}

func TestAdminCommand_RemoveBackend_MissingBackend(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := adminCommand("remove-backend", nil, "http://unused", "tok", &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "backend name is required") {
		t.Errorf("stderr = %q, want 'backend name is required'", stderr.String())
	}
}

func TestAdminCommand_Unknown(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := adminCommand("nonexistent", nil, "http://unused", "tok", &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "unknown admin command") {
		t.Errorf("stderr = %q, want 'unknown admin command'", stderr.String())
	}
}

func TestAdminCommand_Drain_SendsPost(t *testing.T) {
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
	code := adminCommand("drain", []string{"mybackend"}, srv.URL, "secret", &stdout, &stderr)
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

func TestAdminCommand_DrainStatus_SendsGet(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{"active": false})
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := adminCommand("drain-status", []string{"oci"}, srv.URL, "tok", &stdout, &stderr)
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

func TestAdminCommand_DrainCancel_SendsDelete(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "drain cancelled"})
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := adminCommand("drain-cancel", []string{"oci"}, srv.URL, "tok", &stdout, &stderr)
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

func TestAdminCommand_RemoveBackend_SendsDelete(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "backend removed"})
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := adminCommand("remove-backend", []string{"oci"}, srv.URL, "tok", &stdout, &stderr)
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

func TestAdminCommand_RemoveBackend_Purge(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "backend removed"})
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := adminCommand("remove-backend", []string{"-purge", "oci"}, srv.URL, "tok", &stdout, &stderr)
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

func TestAdminCommand_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "backend not found"})
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := adminCommand("drain", []string{"bad"}, srv.URL, "tok", &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "backend not found") {
		t.Errorf("stdout = %q, want to contain 'backend not found'", stdout.String())
	}
}
