// -------------------------------------------------------------------------------
// Admin CLI - Provisioning Command Tests
//
// Author: Alex Freidah
//
// Where each verb sends its request and what it puts in the body, plus the two
// things the rendering owes an operator: every listing marks which entries the
// config file declares, and a minted secret reaches stdout and nothing else.
// -------------------------------------------------------------------------------

package adminctl

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// -------------------------------------------------------------------------
// HARNESS
// -------------------------------------------------------------------------

// capture is what the stub server saw.
type capture struct {
	method string
	path   string
	body   string
}

// runProvisioning drives one command against a stub server answering with
// reply, and returns the exit code, what the server saw, and stdout/stderr.
func runProvisioning(t *testing.T, args []string, reply any) (int, capture, string, string) {
	t.Helper()
	var got capture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = capture{method: r.Method, path: r.URL.Path, body: string(body)}
		_ = json.NewEncoder(w).Encode(reply)
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Command(args[0], args[1:], srv.URL, "secret", &stdout, &stderr)
	return code, got, stdout.String(), stderr.String()
}

// okReply is the acknowledgement shape every mutation answers with.
var okReply = adminapi.ProvisioningOperationResponse{Status: "ok"}

// -------------------------------------------------------------------------
// ROUTING
// -------------------------------------------------------------------------

// TestProvisioning_VerbsAndPaths drives each verb and asserts it reaches the
// route it names, with the verb the route is registered under.
func TestProvisioning_VerbsAndPaths(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		args       []string
		wantMethod string
		wantPath   string
	}{
		{"bucket list", []string{"bucket", "list"}, http.MethodGet, "/admin/api/provisioning"},
		{"bucket create", []string{"bucket", "create", "-name", "photos"},
			http.MethodPost, "/admin/api/provisioning/buckets"},
		{"bucket delete", []string{"bucket", "delete", "-name", "photos"},
			http.MethodDelete, "/admin/api/provisioning/buckets/photos"},
		{"user list", []string{"user", "list"}, http.MethodGet, "/admin/api/provisioning"},
		{"user create", []string{"user", "create", "-name", "ci"},
			http.MethodPost, "/admin/api/provisioning/users"},
		{"user delete", []string{"user", "delete", "-id", "user-abc"},
			http.MethodDelete, "/admin/api/provisioning/users/user-abc"},
		{"credential list", []string{"credential", "list"}, http.MethodGet, "/admin/api/provisioning"},
		{"credential revoke", []string{"credential", "revoke", "-access-key", "AK"},
			http.MethodDelete, "/admin/api/provisioning/credentials/AK"},
		{"grant add", []string{"grant", "add", "-user", "user-abc", "-bucket", "photos"},
			http.MethodPost, "/admin/api/provisioning/grants"},
		{"grant remove", []string{"grant", "remove", "-user", "user-abc", "-bucket", "photos"},
			http.MethodDelete, "/admin/api/provisioning/grants/user-abc/photos"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, got, _, stderr := runProvisioning(t, tc.args, okReply)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr)
			}
			if got.method != tc.wantMethod || got.path != tc.wantPath {
				t.Errorf("request = %s %s, want %s %s", got.method, got.path, tc.wantMethod, tc.wantPath)
			}
		})
	}
}

// TestProvisioning_RequestBodies verifies each creating verb sends what the
// server's wire type expects, since a field named wrong fails silently as a
// zero value.
func TestProvisioning_RequestBodies(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		args []string
		want map[string]any
	}{
		{"bucket", []string{"bucket", "create", "-name", "photos", "-max-multipart", "4"},
			map[string]any{"name": "photos", "max_multipart_uploads": float64(4)}},
		{"user", []string{"user", "create", "-name", "ci"},
			map[string]any{"name": "ci"}},
		{"credential", []string{"credential", "issue", "-user", "user-abc", "-label", "backup job"},
			map[string]any{"user_id": "user-abc", "label": "backup job"}},
		{"grant", []string{"grant", "add", "-user", "user-abc", "-bucket", "photos"},
			map[string]any{"user_id": "user-abc", "bucket": "photos"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, got, _, _ := runProvisioning(t, tc.args, okReply)

			var sent map[string]any
			if err := json.Unmarshal([]byte(got.body), &sent); err != nil {
				t.Fatalf("decode sent body: %v (body=%q)", err, got.body)
			}
			for k, want := range tc.want {
				if sent[k] != want {
					t.Errorf("body[%q] = %v, want %v", k, sent[k], want)
				}
			}
		})
	}
}

// TestProvisioning_MissingRequiredFlags verifies a verb whose required flag is
// absent fails without issuing a request, so a script that forgot an argument
// does not half-apply.
func TestProvisioning_MissingRequiredFlags(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{
		{"bucket", "create"},
		{"bucket", "delete"},
		{"user", "create"},
		{"user", "delete"},
		{"credential", "issue"},
		{"credential", "revoke"},
		{"grant", "add", "-user", "u1"},
		{"grant", "add", "-bucket", "photos"},
		{"grant", "remove", "-user", "u1"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			code, got, _, stderr := runProvisioning(t, args, okReply)
			if code == 0 {
				t.Fatal("a verb missing its required flag succeeded")
			}
			if got.method != "" {
				t.Errorf("issued %s %s despite the missing flag", got.method, got.path)
			}
			if !strings.Contains(stderr, "required") {
				t.Errorf("stderr = %q, want it to name the missing flag", stderr)
			}
		})
	}
}

// -------------------------------------------------------------------------
// SUB-VERB DISPATCH
// -------------------------------------------------------------------------

// TestProvisioning_VerbUsage verifies a noun with no verb fails and lists what
// it accepts, and that asking for help succeeds.
func TestProvisioning_VerbUsage(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		args     []string
		wantCode int
	}{
		{"no verb", []string{"bucket"}, 1},
		{"help", []string{"bucket", "help"}, 0},
		{"unknown verb", []string{"bucket", "frobnicate"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, got, _, stderr := runProvisioning(t, tc.args, okReply)
			if code != tc.wantCode {
				t.Fatalf("exit code = %d, want %d", code, tc.wantCode)
			}
			if got.method != "" {
				t.Errorf("issued %s %s without a verb to run", got.method, got.path)
			}
			for _, want := range []string{"list", "create", "delete"} {
				if !strings.Contains(stderr, want) {
					t.Errorf("usage does not mention %q: %s", want, stderr)
				}
			}
		})
	}
}

// -------------------------------------------------------------------------
// RENDERING
// -------------------------------------------------------------------------

// listingReply is a merged view carrying one entry from each source.
func listingReply() adminapi.ProvisioningResponse {
	return adminapi.ProvisioningResponse{
		Buckets: []adminapi.Bucket{
			{Name: "from-config", Source: adminapi.SourceConfig},
			{Name: "from-store", MaxMultipartUploads: 4, Source: adminapi.SourceStore},
		},
		Users: []adminapi.User{
			{ID: "user-abc", Name: "ci", Buckets: []string{"from-store"}, Source: adminapi.SourceStore},
		},
		Credentials: []adminapi.Credential{
			{AccessKeyID: "AK", UserID: "user-abc", Label: "backup job", Source: adminapi.SourceStore},
		},
	}
}

// TestProvisioning_ListingsMarkTheSource verifies each listing says where an
// entry came from, which is what tells an operator whether it can be changed.
func TestProvisioning_ListingsMarkTheSource(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"bucket", []string{"bucket", "list"}, []string{"from-config", "config", "from-store", "store", "unlimited"}},
		{"user", []string{"user", "list"}, []string{"user-abc", "ci", "from-store", "store"}},
		{"credential", []string{"credential", "list"}, []string{"AK", "user-abc", "backup job", "store"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, _, stdout, stderr := runProvisioning(t, tc.args, listingReply())
			if code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr)
			}
			for _, want := range tc.want {
				if !strings.Contains(stdout, want) {
					t.Errorf("listing omits %q:\n%s", want, stdout)
				}
			}
		})
	}
}

// TestProvisioning_ListingReportsNotices verifies what the merge found reaches
// the operator at the terminal rather than only the server log.
func TestProvisioning_ListingReportsNotices(t *testing.T) {
	t.Parallel()

	reply := listingReply()
	reply.Notices = []adminapi.Notice{{Kind: "dangling_grant", Detail: "grant names bucket \"gone\""}}

	code, _, stdout, stderr := runProvisioning(t, []string{"bucket", "list"}, reply)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "dangling_grant") {
		t.Errorf("listing swallowed the notice:\n%s", stdout)
	}
}

// TestCredentialIssue_PrintsTheSecretToStdout is what the verb exists for: the
// secret appears once, on stdout, so it can be captured into a secret store.
func TestCredentialIssue_PrintsTheSecretToStdout(t *testing.T) {
	t.Parallel()

	code, _, stdout, stderr := runProvisioning(t,
		[]string{"credential", "issue", "-user", "user-abc"},
		adminapi.CreateCredentialResponse{
			AccessKeyID:     "MFRGGZDFMZTWQ2LK",
			SecretAccessKey: "the-only-copy",
			UserID:          "user-abc",
		})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "the-only-copy") || !strings.Contains(stdout, "MFRGGZDFMZTWQ2LK") {
		t.Errorf("stdout omits half the keypair:\n%s", stdout)
	}
	if strings.Contains(stderr, "the-only-copy") {
		t.Error("the secret reached stderr, where a redirect would not capture it")
	}
}

// TestProvisioning_RejectionIsReported verifies a refusal from the server - a
// config-declared entry, a bucket that still holds objects - fails the command
// and carries the reason, rather than exiting zero.
func TestProvisioning_RejectionIsReported(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "declared in the config file and not editable through the API: bucket \"photos\"",
		})
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Command("bucket", []string{"delete", "-name", "photos"}, srv.URL, "secret", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "config file") {
		t.Errorf("stderr = %q, want the server's reason", stderr.String())
	}
}
