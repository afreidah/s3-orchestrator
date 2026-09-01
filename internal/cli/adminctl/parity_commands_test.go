// -------------------------------------------------------------------------------
// Admin CLI - Parity Command Tests (rebalance, encryption, observability)
//
// Author: Alex Freidah
//
// Verifies the commands that bring the CLI to parity with the dashboard route
// to the right admin API endpoint with the right verb and body. trace-snapshot
// is special-cased: its binary payload is written to disk rather than rendered.
// -------------------------------------------------------------------------------

package adminctl

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCommand_ParityVerbsAndPaths drives each simple parity command and asserts
// it issues the expected verb against the expected admin API path.
func TestCommand_ParityVerbsAndPaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		cmd        string
		args       []string
		wantMethod string
		wantPath   string
	}{
		{"rebalance", nil, http.MethodPost, "/admin/api/rebalance"},
		{"lifecycle", nil, http.MethodPost, "/admin/api/lifecycle"},
		{"encrypt-existing", nil, http.MethodPost, "/admin/api/encrypt-existing"},
		{"decrypt-existing", nil, http.MethodPost, "/admin/api/decrypt-existing"},
		{"compress-existing", nil, http.MethodPost, "/admin/api/compress-existing"},
		{"decompress-existing", nil, http.MethodPost, "/admin/api/decompress-existing"},
		{"workers", nil, http.MethodGet, "/admin/api/workers"},
		{"reload-status", nil, http.MethodGet, "/admin/api/reload-status"},
		{"rotate-encryption-key", []string{"-old-key-id", "config-0"}, http.MethodPost, "/admin/api/rotate-encryption-key"},
	}

	for _, tc := range cases {
		t.Run(tc.cmd, func(t *testing.T) {
			t.Parallel()
			var gotMethod, gotPath, gotToken string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath, gotToken = r.Method, r.URL.Path, r.Header.Get("X-Admin-Token")
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
			}))
			defer srv.Close()

			var stdout, stderr bytes.Buffer
			code := Command(tc.cmd, tc.args, srv.URL, "secret", &stdout, &stderr)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
			}
			if gotMethod != tc.wantMethod {
				t.Errorf("method = %q, want %q", gotMethod, tc.wantMethod)
			}
			if gotPath != tc.wantPath {
				t.Errorf("path = %q, want %q", gotPath, tc.wantPath)
			}
			if gotToken != "secret" {
				t.Errorf("token = %q, want secret", gotToken)
			}
		})
	}
}

// TestCommand_RotateEncryptionKey_SendsBody verifies the old key ID is sent as
// the JSON body the server expects.
func TestCommand_RotateEncryptionKey_SendsBody(t *testing.T) {
	t.Parallel()
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "complete"})
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Command("rotate-encryption-key", []string{"-old-key-id", "config-0"}, srv.URL, "tok", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if gotBody["old_key_id"] != "config-0" {
		t.Errorf("body old_key_id = %q, want config-0", gotBody["old_key_id"])
	}
}

// TestCommand_RotateEncryptionKey_RequiresFlag verifies the command fails fast
// with a clear message when -old-key-id is omitted, without hitting the server.
func TestCommand_RotateEncryptionKey_RequiresFlag(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := Command("rotate-encryption-key", nil, "http://unused", "tok", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "-old-key-id is required") {
		t.Errorf("stderr = %q, want '-old-key-id is required'", stderr.String())
	}
}

// TestCommand_FlaggedCommands_RejectBadFlags verifies the flag-parsing commands
// fail with exit 1 on an unknown flag without reaching the server.
func TestCommand_FlaggedCommands_RejectBadFlags(t *testing.T) {
	t.Parallel()
	for _, cmd := range []string{"rotate-encryption-key", "trace-snapshot"} {
		t.Run(cmd, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			code := Command(cmd, []string{"-nonexistent-flag"}, "http://unused", "tok", &stdout, &stderr)
			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
		})
	}
}

// TestCommand_TraceSnapshot_TransportError verifies a connection failure is
// reported and returns exit 1 (the transport-error branch in c.request).
func TestCommand_TraceSnapshot_TransportError(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	// Port 0 on a closed loopback address never accepts a connection.
	code := Command("trace-snapshot", []string{"-o", filepath.Join(t.TempDir(), "t.bin")},
		"http://127.0.0.1:0", "tok", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr=%s", code, stderr.String())
	}
	if stderr.Len() == 0 {
		t.Error("expected a transport error on stderr")
	}
}

// TestCommand_TraceSnapshot_UnwritablePath verifies a write failure (parent
// directory does not exist) is reported and returns exit 1.
func TestCommand_TraceSnapshot_UnwritablePath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("trace"))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	bad := filepath.Join(t.TempDir(), "no-such-dir", "trace.bin")
	code := Command("trace-snapshot", []string{"-o", bad}, srv.URL, "tok", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr=%s", code, stderr.String())
	}
	if stderr.Len() == 0 {
		t.Error("expected a write error on stderr")
	}
}

// TestCommand_TraceSnapshot_WritesFile verifies the binary trace payload is
// written verbatim to the -o path and a byte-count confirmation is printed.
func TestCommand_TraceSnapshot_WritesFile(t *testing.T) {
	t.Parallel()
	payload := []byte("go-trace-binary-bytes")
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "trace.bin")
	var stdout, stderr bytes.Buffer
	code := Command("trace-snapshot", []string{"-o", out}, srv.URL, "tok", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if gotMethod != http.MethodPost || gotPath != "/admin/api/trace/snapshot" {
		t.Errorf("got %s %s, want POST /admin/api/trace/snapshot", gotMethod, gotPath)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("written bytes = %q, want %q", got, payload)
	}
	if !strings.Contains(stdout.String(), "wrote 21 bytes to "+out) {
		t.Errorf("stdout = %q, want byte-count confirmation", stdout.String())
	}
}

// TestCommand_TraceSnapshot_DisabledRendersError verifies a 503 (recorder off)
// is rendered through the error path and no file is written.
func TestCommand_TraceSnapshot_DisabledRendersError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "flight recorder is disabled"})
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "trace.bin")
	var stdout, stderr bytes.Buffer
	code := Command("trace-snapshot", []string{"-o", out}, srv.URL, "tok", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "flight recorder is disabled") {
		t.Errorf("stderr = %q, want disabled message", stderr.String())
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("file should not be written on error, stat err = %v", err)
	}
}
