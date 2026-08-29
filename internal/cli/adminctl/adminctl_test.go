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
	"os"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/cli/admintarget"
	"github.com/afreidah/s3-orchestrator/internal/cli/output"
)

// TestMain clears the S3O_ADMIN_* environment variables before the suite runs.
// A developer's exported admin addr/token would otherwise leak into the tests
// that exercise config-file loading, which Run skips whenever those env vars
// are set.
func TestMain(m *testing.M) {
	os.Unsetenv(admintarget.EnvAddr)
	os.Unsetenv(admintarget.EnvToken)
	os.Exit(m.Run())
}

// TestRun_FlagTarget drives Run end-to-end against a fake server using only
// -addr/-token, proving the admin CLI works with no config file present.
func TestRun_FlagTarget(t *testing.T) {
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Admin-Token")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	code := Run([]string{"-addr", srv.URL, "-token", "tok", "status"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errb.String())
	}
	if gotToken != "tok" {
		t.Errorf("server saw token %q, want tok", gotToken)
	}
}

// TestCommand_Drain_MissingBackend verifies the command drain missing backend contract.
// Asserts that exit code = , want 1.
func TestCommand_Drain_MissingBackend(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := Command("drain", nil, "http://unused", "tok", &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "backend name is required") {
		t.Errorf("stderr = %q, want 'backend name is required'", stderr.String())
	}
}

// TestCommand_DrainStatus_MissingBackend verifies the command drain status missing backend contract.
// Asserts that exit code = , want 1.
func TestCommand_DrainStatus_MissingBackend(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := Command("drain-status", nil, "http://unused", "tok", &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "backend name is required") {
		t.Errorf("stderr = %q, want 'backend name is required'", stderr.String())
	}
}

// TestCommand_DrainCancel_MissingBackend verifies the command drain cancel missing backend contract.
// Asserts that exit code = , want 1.
func TestCommand_DrainCancel_MissingBackend(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := Command("drain-cancel", nil, "http://unused", "tok", &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "backend name is required") {
		t.Errorf("stderr = %q, want 'backend name is required'", stderr.String())
	}
}

// TestCommand_RemoveBackend_MissingBackend verifies the command remove backend missing backend contract.
// Asserts that exit code = , want 1.
func TestCommand_RemoveBackend_MissingBackend(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := Command("remove-backend", nil, "http://unused", "tok", &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "backend name is required") {
		t.Errorf("stderr = %q, want 'backend name is required'", stderr.String())
	}
}

// TestCommand_Unknown verifies the command unknown contract.
// Asserts that exit code = , want 1.
func TestCommand_Unknown(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := Command("nonexistent", nil, "http://unused", "tok", &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "unknown admin command") {
		t.Errorf("stderr = %q, want 'unknown admin command'", stderr.String())
	}
}

// TestCommand_Drain_SendsPost verifies the command drain sends post contract.
// Asserts that exit code = , want 0.
func TestCommand_Drain_SendsPost(t *testing.T) {
	t.Parallel()
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

// TestCommand_DrainStatus_SendsGet verifies the command drain status sends get contract.
// Asserts that exit code = , want 0.
func TestCommand_DrainStatus_SendsGet(t *testing.T) {
	t.Parallel()
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

// TestCommand_DrainCancel_SendsDelete verifies the command drain cancel sends delete contract.
// Asserts that exit code = , want 0.
func TestCommand_DrainCancel_SendsDelete(t *testing.T) {
	t.Parallel()
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

// TestCommand_RemoveBackend_SendsDelete verifies the command remove backend sends delete contract.
// Asserts that exit code = , want 0.
func TestCommand_RemoveBackend_SendsDelete(t *testing.T) {
	t.Parallel()
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

// TestCommand_RemoveBackend_Purge verifies the command remove backend purge contract.
// Asserts that exit code = , want 0.
func TestCommand_RemoveBackend_Purge(t *testing.T) {
	t.Parallel()
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

// TestCommand_Reconcile_DefaultPostsAllBackends verifies the command reconcile default posts all backends contract.
// Asserts that exit code = , want 0.
func TestCommand_Reconcile_DefaultPostsAllBackends(t *testing.T) {
	t.Parallel()
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

// TestCommand_Reconcile_ScopesToBackend verifies the command reconcile scopes to backend contract.
// Asserts that exit code = , want 0.
func TestCommand_Reconcile_ScopesToBackend(t *testing.T) {
	t.Parallel()
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

// TestCommand_ServerError verifies the command server error contract.
// Asserts that exit code = , want 1.
func TestCommand_ServerError(t *testing.T) {
	t.Parallel()
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
	// Text mode renders the error as a single "error: <message>" line on stderr.
	if !strings.Contains(stderr.String(), "backend not found") {
		t.Errorf("stderr = %q, want to contain 'backend not found'", stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want empty on text-mode error", stdout.String())
	}
}

// TestCommand_ServerErrorJSON verifies that --json renders an error body as raw
// JSON on stdout (the machine-readable contract) rather than the stderr line.
func TestCommand_ServerErrorJSON(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "backend not found"})
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := CommandWithFormat("drain", []string{"bad"}, srv.URL, "tok", output.FormatJSON, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "backend not found") {
		t.Errorf("stdout = %q, want JSON error body", stdout.String())
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
// wrapperCase carries one row of the simple-wrapper assertion table.
type wrapperCase struct {
	name        string
	cmd         string
	args        []string
	wantMethod  string
	wantPath    string
	wantQueryIn string // substring match (some commands optionally append ?...)
}

// simpleWrapperCases enumerates the flag-shape variants for every plain
// HTTP wrapper. Keeping the cmd directly on each row removes the
// case-name -> cmd-name switch that pushed the test over the cognitive
// complexity threshold.
var simpleWrapperCases = []wrapperCase{
	{"status", "status", nil, http.MethodGet, "/admin/api/status", ""},
	{"cleanup-queue", "cleanup-queue", nil, http.MethodGet, "/admin/api/cleanup-queue", ""},
	{"usage-flush", "usage-flush", nil, http.MethodPost, "/admin/api/usage-flush", ""},
	{"usage-reconcile", "usage-reconcile", nil, http.MethodPost, "/admin/api/usage-reconcile", ""},
	{"replicate", "replicate", nil, http.MethodPost, "/admin/api/replicate", ""},
	{"over-replication", "over-replication", nil, http.MethodGet, "/admin/api/over-replication", ""},
	{"over-replication-execute", "over-replication", []string{"-execute"}, http.MethodPost, "/admin/api/over-replication", ""},
	{"over-replication-execute-batch", "over-replication", []string{"-execute", "-batch-size", "200"}, http.MethodPost, "/admin/api/over-replication", "batch_size=200"},
	{"log-level-get", "log-level", nil, http.MethodGet, "/admin/api/log-level", ""},
	{"log-level-set", "log-level", []string{"-set", "debug"}, http.MethodPut, "/admin/api/log-level", ""},
	{"scrub", "scrub", nil, http.MethodPost, "/admin/api/scrub", ""},
	{"scrub-batch", "scrub", []string{"-batch-size", "50"}, http.MethodPost, "/admin/api/scrub", "batch_size=50"},
	{"scrub-key", "scrub", []string{"-key", "bucket/a b"}, http.MethodPost, "/admin/api/object-scrub", "key=bucket%2Fa+b"},
	{"backfill-checksums", "backfill-checksums", nil, http.MethodPost, "/admin/api/backfill-checksums", ""},
	{"backfill-checksums-batch", "backfill-checksums", []string{"-batch-size", "50"}, http.MethodPost, "/admin/api/backfill-checksums", "batch_size=50"},
	{"backfill-checksums-max", "backfill-checksums", []string{"-max", "200"}, http.MethodPost, "/admin/api/backfill-checksums", "max=200"},
	{"backfill-checksums-delay", "backfill-checksums", []string{"-delay-ms", "500"}, http.MethodPost, "/admin/api/backfill-checksums", "delay_ms=500"},
	{"backfill-checksums-all", "backfill-checksums", []string{"-batch-size", "50", "-max", "200", "-delay-ms", "500"}, http.MethodPost, "/admin/api/backfill-checksums", "batch_size=50&delay_ms=500&max=200"},
	{"object-locations", "object-locations", []string{"-key", "my/key"}, http.MethodGet, "/admin/api/object-locations", "key=my/key"},
	// The key is escaped on the way out so a key carrying "?" or "#" reaches
	// the server intact; the server decodes it again, so the path recorded
	// here is the decoded form.
	{"object-tags-read", "object-tags", []string{"-key", "bucket/k"}, http.MethodGet, "/admin/api/objects/tags/bucket/k", ""},
	{"object-tags-set", "object-tags", []string{"-key", "bucket/k", "-tag", "a=1"}, http.MethodPut, "/admin/api/objects/tags/bucket/k", ""},
	{"object-tags-clear", "object-tags", []string{"-key", "bucket/k", "-clear"}, http.MethodDelete, "/admin/api/objects/tags/bucket/k", ""},
}

// runWrapperCase exercises a single wrapper assertion. Pulling the body out
// of the loop keeps TestCommand_SimpleGetAndPostWrappers below the
// cognitive-complexity threshold (Sonar S3776).
func runWrapperCase(t *testing.T, tc *wrapperCase) {
	t.Helper()
	var gotMethod, gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		jsonOK(w, r)
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Command(tc.cmd, tc.args, srv.URL, "tok", &stdout, &stderr)
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
}

// TestCommand_SimpleGetAndPostWrappers walks every handler that is a
// one-line wrapper around doGet/doPost so the matrix is covered without
// duplicating boilerplate per command.
func TestCommand_SimpleGetAndPostWrappers(t *testing.T) {
	t.Parallel()
	for i := range simpleWrapperCases {
		tc := &simpleWrapperCases[i]
		t.Run(tc.name, func(t *testing.T) { runWrapperCase(t, tc) })
	}
}

// TestCommand_ObjectLocations_MissingKey covers the early-exit branch of
// cmdObjectLocations where the required -key flag is not provided.
func TestCommand_ObjectLocations_MissingKey(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// minimalAdminYAML is the smallest valid config blob admin tests
// need to reach the dispatcher; only the server.admin_token field is
// load-bearing for the auth path under test.
const minimalAdminYAML = `
server:
  listen_addr: "%ADDR%"
database:
  driver: sqlite
  path: ":memory:"
buckets:
  - name: test
    credentials:
      - access_key_id: ak
        secret_access_key: sk
backends:
  - name: b1
    endpoint: http://localhost:19000
    region: us-east-1
    bucket: bucket1
    access_key_id: ak
    secret_access_key: sk
ui:
  admin_token: "%TOKEN%"
`

// writeAdminConfig drops a config file with the given listener address and
// admin token so Run() finds enough state to dispatch.
func writeAdminConfig(t *testing.T, addr, token string) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/config.yaml"
	body := strings.NewReplacer("%ADDR%", addr, "%TOKEN%", token).Replace(minimalAdminYAML)
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestRun_Help drives the no-args branch which prints usage and exits 0.
func TestRun_Help(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := Run([]string{}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stderr.String(), "Usage: s3-orchestrator admin") {
		t.Errorf("stderr = %q, want usage line", stderr.String())
	}
}

// TestRun_BadConfigPath covers the LoadConfig-error branch.
func TestRun_BadConfigPath(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := Run([]string{"-config", "/no/such/file.yaml", "status"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

// TestRun_MissingAdminToken covers the empty-token branch where the config
// loads cleanly but no admin_token / admin_key was set.
func TestRun_MissingAdminToken(t *testing.T) {
	t.Parallel()
	path := writeAdminConfig(t, "127.0.0.1:9999", "")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"-config", path, "status"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "admin_token") {
		t.Errorf("stderr = %q, want admin_token error", stderr.String())
	}
}

// TestRun_DispatchesViaConfigAddr drives the full Run path: config is read,
// the address is auto-prefixed with http://, the token comes from
// admin_token, and the dispatch reaches the live test server.
func TestRun_DispatchesViaConfigAddr(t *testing.T) {
	t.Parallel()
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Admin-Token")
		jsonOK(w, r)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	path := writeAdminConfig(t, addr, "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"-config", path, "status"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", code, stderr.String())
	}
	if gotToken != "tok" {
		t.Errorf("token = %q, want tok", gotToken)
	}
}

// TestRun_AddrFlagOverridesConfig drives the -addr override branch and the
// already-prefixed http:// path.
func TestRun_AddrFlagOverridesConfig(t *testing.T) {
	t.Parallel()
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		jsonOK(w, r)
	}))
	defer srv.Close()

	path := writeAdminConfig(t, "127.0.0.1:9999", "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"-config", path, "-addr", srv.URL, "status"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", code, stderr.String())
	}
	if !hit {
		t.Error("server was not hit; -addr override likely ignored")
	}
}

// TestCommand_TransportError covers the connection-failure branch of
// doRequest by pointing at a closed listener.
func TestCommand_TransportError(t *testing.T) {
	t.Parallel()
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

// TestCommand_CacheFlush_SendsPost verifies the cache-flush subcommand
// POSTs to /admin/api/cache/flush with the admin token.
func TestCommand_CacheFlush_SendsPost(t *testing.T) {
	t.Parallel()
	var gotMethod, gotPath, gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotToken = r.Header.Get("X-Admin-Token")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "flushed", "entries_dropped": 0})
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Command("cache-flush", nil, srv.URL, "tok", &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if gotMethod != http.MethodPost || gotPath != "/admin/api/cache/flush" {
		t.Errorf("got %s %s, want POST /admin/api/cache/flush", gotMethod, gotPath)
	}
	if gotToken != "tok" {
		t.Errorf("token = %q, want tok", gotToken)
	}
}

// TestCommand_CacheStats_SendsGet verifies the cache-stats subcommand
// GETs the cache stats endpoint and prints the response.
func TestCommand_CacheStats_SendsGet(t *testing.T) {
	t.Parallel()
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{"entries": 5, "bytes": 1024})
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Command("cache-stats", nil, srv.URL, "tok", &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if gotMethod != http.MethodGet || gotPath != "/admin/api/cache" {
		t.Errorf("got %s %s, want GET /admin/api/cache", gotMethod, gotPath)
	}
	if !strings.Contains(stdout.String(), "entries") {
		t.Errorf("stdout missing response body: %s", stdout.String())
	}
}

// TestCommand_CacheInvalidate_SendsDelete verifies the cache-invalidate
// subcommand DELETEs /admin/api/cache/keys/<key> using the supplied
// -key flag.
func TestCommand_CacheInvalidate_SendsDelete(t *testing.T) {
	t.Parallel()
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "invalidated"})
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Command("cache-invalidate", []string{"-key=photos/foo.jpg"}, srv.URL, "tok", &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	if gotPath != "/admin/api/cache/keys/photos/foo.jpg" {
		t.Errorf("path = %q, want /admin/api/cache/keys/photos/foo.jpg", gotPath)
	}
}

// TestCommand_CacheInvalidate_MissingKey verifies that omitting -key
// exits non-zero and reports the missing flag to stderr.
func TestCommand_CacheInvalidate_MissingKey(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := Command("cache-invalidate", nil, "http://unused.invalid", "tok", &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "-key is required") {
		t.Errorf("stderr = %q, want to mention -key is required", stderr.String())
	}
}

// TestCommand_CacheInvalidatePrefix_SendsDelete verifies the
// cache-invalidate-prefix subcommand DELETEs the prefix endpoint with
// the -prefix flag URL-escaped into the query string.
func TestCommand_CacheInvalidatePrefix_SendsDelete(t *testing.T) {
	t.Parallel()
	var gotMethod, gotPath, gotPrefix string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotPrefix = r.URL.Query().Get("prefix")
		_ = json.NewEncoder(w).Encode(map[string]any{"entries_dropped": 3})
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Command("cache-invalidate-prefix", []string{"-prefix=users/1/"}, srv.URL, "tok", &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if gotMethod != http.MethodDelete || gotPath != "/admin/api/cache/prefix" {
		t.Errorf("got %s %s, want DELETE /admin/api/cache/prefix", gotMethod, gotPath)
	}
	if gotPrefix != "users/1/" {
		t.Errorf("prefix = %q, want users/1/", gotPrefix)
	}
}

// TestCommand_CacheInvalidatePrefix_MissingPrefix verifies that omitting
// -prefix exits non-zero and reports the missing flag to stderr.
func TestCommand_CacheInvalidatePrefix_MissingPrefix(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := Command("cache-invalidate-prefix", nil, "http://unused.invalid", "tok", &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "-prefix is required") {
		t.Errorf("stderr = %q, want to mention -prefix is required", stderr.String())
	}
}

// TestErrorMessage verifies extraction of the "error" field and the fallback
// to the trimmed raw body when the response is not the expected shape.
func TestErrorMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want string
	}{
		{"error field", `{"error":"boom"}`, "boom"},
		{"no error field", `{"status":"weird"}`, `{"status":"weird"}`},
		{"empty error field", `{"error":""}`, `{"error":""}`},
		{"non-json", "  plain text  ", "plain text"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := errorMessage([]byte(tc.body)); got != tc.want {
				t.Errorf("errorMessage(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// TestCommand_TextVsJSONRendering verifies the format split on a generic
// (no bespoke renderer) command: text mode renders a human "key: value" view,
// JSON mode pretty-prints the raw body with indentation.
func TestCommand_TextVsJSONRendering(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"entries":3,"capacity":100}`))
	}))
	defer srv.Close()

	var textOut, textErr bytes.Buffer
	if code := Command("cache-stats", nil, srv.URL, "tok", &textOut, &textErr); code != 0 {
		t.Fatalf("text exit = %d (stderr=%q)", code, textErr.String())
	}
	if !strings.Contains(textOut.String(), "entries: 3") {
		t.Errorf("text output = %q, want human key/value", textOut.String())
	}
	if strings.Contains(textOut.String(), "{") {
		t.Errorf("text output should not contain JSON braces: %q", textOut.String())
	}

	var jsonOut, jsonErr bytes.Buffer
	if code := CommandWithFormat("cache-stats", nil, srv.URL, "tok", output.FormatJSON, &jsonOut, &jsonErr); code != 0 {
		t.Fatalf("json exit = %d (stderr=%q)", code, jsonErr.String())
	}
	if !strings.Contains(jsonOut.String(), "{\n  \"") {
		t.Errorf("json output = %q, want indented JSON", jsonOut.String())
	}
}

// TestCommand_FlagParseErrors verifies that every flag-parsing command exits 1
// on an unknown flag rather than reaching the network.
func TestCommand_FlagParseErrors(t *testing.T) {
	t.Parallel()
	cmds := []string{
		"object-locations", "over-replication", "log-level", "scrub",
		"backfill-checksums", "reconcile", "remove-backend",
		"cache-invalidate", "cache-invalidate-prefix",
	}
	for _, cmd := range cmds {
		t.Run(cmd, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			code := Command(cmd, []string{"-nonexistent-flag"}, "http://unused.invalid", "tok", &stdout, &stderr)
			if code != 1 {
				t.Errorf("exit code = %d, want 1 on unknown flag", code)
			}
		})
	}
}
