// -------------------------------------------------------------------------------
// Admin API - Signed Request Round Trip
//
// Author: Alex Freidah
//
// Drives the real admin mux over HTTP with the real client, so the canonical
// request the client signs and the one the server reconstructs are compared by
// the code that will compare them in production.
//
// Each half is already covered alone. What is not covered alone is agreement
// between them: a difference in query encoding, in the payload hash, or in
// which headers the signature covers refuses every signed request identically,
// and neither side's own tests would show it.
// -------------------------------------------------------------------------------

package admin

import (
	"cmp"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/provisioning"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
)

// The keypair the signed tests present.
const (
	signedAccessKey = "AKIASIGNEDTEST"
	signedSecret    = "signed-test-secret"
)

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// signedServer mounts the real admin mux over HTTP with one keypair registered
// against a user holding the given permissions, and returns its address.
//
// Only the routes that need no collaborators are exercised, so the handler is
// assembled bare: what is under test is the decision the guard reaches, not the
// work behind it.
func signedServer(t *testing.T, admin map[core.Resource]core.PermissionSet) string {
	t.Helper()
	view := provisioning.View{
		Buckets: []provisioning.Bucket{{Name: grantedBucket, Source: provisioning.SourceStore}},
		Users: []provisioning.User{{
			ID:     "u1",
			Name:   "operator",
			Admin:  admin,
			Source: provisioning.SourceStore,
		}},
		Credentials: []provisioning.Credential{{
			AccessKeyID: signedAccessKey,
			UserID:      "u1",
			Secret:      signedSecret,
			Source:      provisioning.SourceStore,
		}},
	}
	registry, err := auth.NewBucketRegistry(&view)
	if err != nil {
		t.Fatalf("NewBucketRegistry: %v", err)
	}

	var lv slog.LevelVar
	h := &Handler{
		log:      slog.Default().With(logfmt.Component("admin")),
		token:    "test-token",
		registry: func() *auth.BucketRegistry { return registry },
		logLevel: &lv,
	}
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// statusOf issues one signed request through the real client and reports the
// status the server answered with.
func statusOf(t *testing.T, addr, accessKey, secret, method, path string, body string) int {
	t.Helper()
	c := adminclient.NewSigned(addr, accessKey, secret)
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	var resp *http.Response
	var err error
	if reader == nil {
		resp, err = c.Do(context.Background(), method, path, nil, nil)
	} else {
		resp, err = c.Do(context.Background(), method, path, nil, reader)
	}
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestSigned_RoundTripAuthenticates is the agreement test: a request the client
// signs verifies against the server without either side being adjusted for the
// other.
func TestSigned_RoundTripAuthenticates(t *testing.T) {
	t.Parallel()

	addr := signedServer(t, onInstance(core.PermAdminRead))
	if got := statusOf(t, addr, signedAccessKey, signedSecret,
		http.MethodGet, "/admin/api/log-level", ""); got != http.StatusOK {
		t.Errorf("status = %d, want 200; the signed request did not verify", got)
	}
}

// TestSigned_QueryIsCanonicalised pins that a request carrying query parameters
// still verifies. The canonical query is sorted and escaped before it is
// signed, so a client and server that build it differently agree on the empty
// case and disagree here.
//
// The parameters are deliberately out of order and carry a character that has
// to be escaped, which is what the two sides have to agree about. The route
// reads none of them; what is under test is the signature, not the handler.
func TestSigned_QueryIsCanonicalised(t *testing.T) {
	t.Parallel()

	addr := signedServer(t, onInstance(core.PermAdminRead))
	if got := statusOf(t, addr, signedAccessKey, signedSecret,
		http.MethodGet, "/admin/api/log-level?b=2&a=1&c=hello+world", ""); got != http.StatusOK {
		t.Errorf("status = %d, want 200; a query parameter broke the signature", got)
	}
}

// TestSigned_BodyIsCovered pins that a request carrying a JSON body verifies.
// The signature covers the body's hash, so a client that signs a different
// number of bytes than it sends is refused.
func TestSigned_BodyIsCovered(t *testing.T) {
	t.Parallel()

	addr := signedServer(t, onInstance(core.PermAdminConfig))
	if statusOf(t, addr, signedAccessKey, signedSecret,
		http.MethodPut, "/admin/api/log-level", `{"level":"debug"}`) == http.StatusUnauthorized {
		t.Error("status = 401; the request body broke the signature")
	}
}

// TestSigned_WrongSecretIsRefused verifies the signature is actually checked. A
// round trip that passes whatever it is handed would satisfy every other test
// here.
func TestSigned_WrongSecretIsRefused(t *testing.T) {
	t.Parallel()

	addr := signedServer(t, onInstance(core.PermAdminRead))
	for _, tc := range []struct {
		name   string
		key    string
		secret string
	}{
		{"wrong secret", signedAccessKey, "not-the-secret"},
		{"unknown access key", "AKIANOSUCHKEY", signedSecret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := statusOf(t, addr, tc.key, tc.secret,
				http.MethodGet, "/admin/api/log-level", ""); got != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", got)
			}
		})
	}
}

// TestSigned_ResolvesToItsGrants verifies a signed credential is held to what it
// holds rather than merely to having authenticated. Without this, signing would
// be a second way to reach everything.
func TestSigned_ResolvesToItsGrants(t *testing.T) {
	t.Parallel()

	addr := signedServer(t, onInstance(core.PermAdminRead))
	for _, tc := range []struct {
		name   string
		method string
		path   string
		want   int
	}{
		{"the granted permission", http.MethodGet, "/admin/api/log-level", http.StatusOK},
		{"a permission it does not hold", http.MethodPut, "/admin/api/log-level", http.StatusForbidden},
		{"an unrelated control-plane permission", http.MethodPost, "/admin/api/rotate-encryption-key", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := ""
			if tc.method == http.MethodPut {
				body = `{"level":"debug"}`
			}
			if got := statusOf(t, addr, signedAccessKey, signedSecret, tc.method, tc.path, body); got != tc.want {
				t.Errorf("status = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestSigned_UploadUsesUnsignedPayload verifies the streamed-upload path still
// authenticates. It signs UNSIGNED-PAYLOAD rather than a body hash, because
// hashing would mean holding the whole object, so it is the one path whose
// signature is built differently from every other.
func TestSigned_UploadUsesUnsignedPayload(t *testing.T) {
	t.Parallel()

	addr := signedServer(t, nil)
	c := adminclient.NewSigned(addr, signedAccessKey, signedSecret)
	body := strings.NewReader("hello")
	resp, err := c.Upload(context.Background(), http.MethodPut,
		"/admin/api/objects/"+grantedBucket+"/k", nil, body, 5, "application/octet-stream")
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// The user holds no bucket grant, so this is refused on authorization. A
	// signature the server could not verify would refuse it earlier, with 401.
	if resp.StatusCode == http.StatusUnauthorized {
		t.Error("status = 401; the unsigned-payload upload did not verify")
	}
}

// TestSigned_ConfigDeclaresTheRootCredential verifies the configured root
// credential signs, which is the path that replaces the shared token.
func TestSigned_ConfigDeclaresTheRootCredential(t *testing.T) {
	t.Parallel()

	view := provisioning.Merge(nil, config.AuthConfig{
		Root: config.RootCredential{AccessKeyID: signedAccessKey, SecretAccessKey: signedSecret},
	}, &provisioning.Snapshot{})
	registry, err := auth.NewBucketRegistry(&view)
	if err != nil {
		t.Fatalf("NewBucketRegistry: %v", err)
	}

	var lv slog.LevelVar
	h := &Handler{
		log:      slog.Default().With(logfmt.Component("admin")),
		registry: func() *auth.BucketRegistry { return registry },
		logLevel: &lv,
	}
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	if got := statusOf(t, srv.URL, signedAccessKey, signedSecret,
		http.MethodGet, "/admin/api/log-level", ""); got != http.StatusOK {
		t.Errorf("status = %d, want 200; the root credential did not reach the control plane", got)
	}
}

// TestSharedToken_ResolvesToRoot pins the path an existing deployment is on:
// a config declaring only ui.admin_token, with no root keypair, still has its
// token reach the control plane.
//
// The token no longer authorizes by itself - it resolves to the root user and
// that user's grants answer - so a config that produced no root user would
// refuse every admin request. This is the demo's configuration.
func TestSharedToken_ResolvesToRoot(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		ui   config.UIConfig
	}{
		{"admin_token set", config.UIConfig{AdminToken: "admin"}},
		{"only admin_key set", config.UIConfig{AdminKey: "admin", AdminSecret: "admin"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{UI: tc.ui}
			// The fallback from admin_token to admin_key lives in validation, so
			// the identity is built from whichever the operator declared.
			cfg.Auth.LegacySharedToken = cmp.Or(cfg.UI.AdminToken, cfg.UI.AdminKey)

			view := provisioning.Merge(nil, cfg.Auth, &provisioning.Snapshot{})
			registry, err := auth.NewBucketRegistry(&view)
			if err != nil {
				t.Fatalf("NewBucketRegistry: %v", err)
			}
			var lv slog.LevelVar
			h := &Handler{
				log:      slog.Default().With(logfmt.Component("admin")),
				token:    cfg.Auth.LegacySharedToken,
				registry: func() *auth.BucketRegistry { return registry },
				logLevel: &lv,
			}
			mux := http.NewServeMux()
			h.Register(mux)

			w := httptest.NewRecorder()
			mux.ServeHTTP(w, doToken("admin", http.MethodGet, "/admin/api/log-level", ""))
			if w.Code != http.StatusOK {
				t.Errorf("status = %d, want 200; the shared token reached nobody", w.Code)
			}
		})
	}
}
