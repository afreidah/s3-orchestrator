// -------------------------------------------------------------------------------
// Admin API - Authorization Tests
//
// Author: Alex Freidah
//
// Covers what a credential reaches on the admin surface: a provisioned one is
// held to the grants it carries on the objects it names, the configured admin
// token still reaches everything, and neither reaches what the other is for.
//
// The object operations are mocked at the store, so these tests assert the
// decision rather than the work it guards - a refused request is one the store
// never sees.
// -------------------------------------------------------------------------------

package admin

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/provisioning"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"

	"go.uber.org/mock/gomock"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// The credential the grant tests authenticate with, and the bucket it holds a
// grant on.
const (
	grantedToken  = "granted-token"
	grantedBucket = "photos"
)

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// registryGranting builds a registry holding one token credential whose user
// reaches grantedBucket with the given permissions.
func registryGranting(t *testing.T, perms core.PermissionSet) *auth.BucketRegistry {
	t.Helper()
	view := provisioning.View{
		Buckets: []provisioning.Bucket{{Name: grantedBucket, Source: provisioning.SourceStore}},
		Users: []provisioning.User{{
			ID:      "u1",
			Name:    "operator",
			Buckets: []string{grantedBucket},
			Grants:  map[string]core.PermissionSet{grantedBucket: perms},
			Source:  provisioning.SourceStore,
		}},
		Credentials: []provisioning.Credential{{
			AccessKeyID: "AKIATEST",
			UserID:      "u1",
			Token:       grantedToken,
			Source:      provisioning.SourceStore,
		}},
	}
	registry, err := auth.NewBucketRegistry(&view)
	if err != nil {
		t.Fatalf("NewBucketRegistry: %v", err)
	}
	return registry
}

// authzMux builds a handler whose object operations read the given mock store
// and whose registry holds the granted credential, mounted on its own mux.
func authzMux(t *testing.T, mock core.ObjectStore, perms core.PermissionSet) *http.ServeMux {
	t.Helper()
	cb := store.NewDatabaseBreaker(config.CircuitBreakerConfig{FailureThreshold: 3})
	var lv slog.LevelVar
	registry := registryGranting(t, perms)
	h := &Handler{
		log:       slog.Default().With(logfmt.Component("admin")),
		dbHealthy: cb.IsHealthy,
		objects:   objectsOver(t, mock),
		token:     "test-token",
		registry:  func() *auth.BucketRegistry { return registry },
		logLevel:  &lv,
	}
	mux := http.NewServeMux()
	h.Register(mux)
	return mux
}

// doToken builds a request carrying the given admin token value.
func doToken(token, method, path, body string) *http.Request {
	req := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
	req.Header.Set(adminTokenHeader, token)
	return req
}

// serveAs runs one request through a handler granting perms and reports the
// status. The store is a strict mock with no expectations, so a request that
// reaches an object operation fails the test rather than passing quietly.
func serveAs(t *testing.T, perms core.PermissionSet, token, method, target string) int {
	t.Helper()
	mock := storetest.NewMockObjectStore(gomock.NewController(t))
	w := httptest.NewRecorder()
	authzMux(t, mock, perms).ServeHTTP(w, doToken(token, method, target, ""))
	return w.Code
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestAuthz_GrantDoesNotCarryPermission refuses each object operation the
// credential's grant leaves out, which is the hole this closes: an admin
// credential used to reach every operation on every bucket.
func TestAuthz_GrantDoesNotCarryPermission(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		held   core.PermissionSet
		method string
		target string
	}{
		{"read without read", core.PermList, http.MethodGet, "/admin/api/objects/photos/cat.jpg"},
		{"write without write", core.PermRead, http.MethodPut, "/admin/api/objects/photos/cat.jpg"},
		{"delete without delete", core.PermRead, http.MethodDelete, "/admin/api/objects/photos/cat.jpg"},
		{"delete prefix without delete", core.PermRead, http.MethodDelete, "/admin/api/objects?prefix=photos/"},
		{"tags without tags", core.PermRead, http.MethodGet, "/admin/api/objects/tags/photos/cat.jpg"},
		{"list without list", core.PermRead, http.MethodGet, "/admin/api/objects?prefix=photos/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := serveAs(t, tc.held, grantedToken, tc.method, tc.target); got != http.StatusForbidden {
				t.Errorf("status = %d, want 403", got)
			}
		})
	}
}

// TestAuthz_BucketNotGranted refuses a bucket the credential holds no grant on,
// even when its grant elsewhere carries every permission.
func TestAuthz_BucketNotGranted(t *testing.T) {
	t.Parallel()

	if got := serveAs(t, core.PermAll, grantedToken, http.MethodGet, "/admin/api/objects/other/cat.jpg"); got != http.StatusForbidden {
		t.Errorf("status = %d, want 403", got)
	}
}

// TestAuthz_BucketlessPrefixNeedsTheAdminToken refuses a prefix naming no
// single bucket. The empty prefix is the whole namespace and a partial name
// spans every bucket it prefixes, so neither can be authorized against one
// grant.
func TestAuthz_BucketlessPrefixNeedsTheAdminToken(t *testing.T) {
	t.Parallel()

	for _, target := range []string{
		"/admin/api/objects?prefix=",
		"/admin/api/objects?prefix=pho",
		"/admin/api/objects?prefix=&delimiter=",
	} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			if got := serveAs(t, core.PermAll, grantedToken, http.MethodGet, target); got != http.StatusForbidden {
				t.Errorf("status = %d, want 403", got)
			}
		})
	}
}

// TestAuthz_ControlPlaneRefusesProvisionedCredential pins that a credential
// carrying bucket grants reaches no fleet operation. Bucket grants say nothing
// about draining a backend, so an absent permission on a control-plane route
// must not read as "no check needed".
func TestAuthz_ControlPlaneRefusesProvisionedCredential(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		method string
		target string
	}{
		{http.MethodGet, "/admin/api/status"},
		{http.MethodPost, "/admin/api/rotate-encryption-key"},
		{http.MethodDelete, "/admin/api/backends/b1"},
		{http.MethodPost, "/admin/api/provisioning/users"},
	} {
		t.Run(tc.target, func(t *testing.T) {
			t.Parallel()
			if got := serveAs(t, core.PermAll, grantedToken, tc.method, tc.target); got != http.StatusForbidden {
				t.Errorf("status = %d, want 403", got)
			}
		})
	}
}

// TestAuthz_UnknownTokenIsUnauthenticated separates a credential that proved
// nothing from one that proved an identity holding too little: the first is a
// 401, the second a 403.
func TestAuthz_UnknownTokenIsUnauthenticated(t *testing.T) {
	t.Parallel()

	for _, token := range []string{"", "not-a-token"} {
		if got := serveAs(t, core.PermAll, token, http.MethodGet, "/admin/api/objects/photos/cat.jpg"); got != http.StatusUnauthorized {
			t.Errorf("token %q: status = %d, want 401", token, got)
		}
	}
}

// TestAuthz_AdminTokenStillReachesEverything pins the fallback an existing
// deployment relies on. It is deprecated rather than removed, so every route it
// reached before this change still answers.
func TestAuthz_AdminTokenStillReachesEverything(t *testing.T) {
	t.Parallel()

	mock := storetest.NewMockObjectStore(gomock.NewController(t))
	mock.EXPECT().ListObjectsDelimited(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&core.ListDelimitedResult{}, nil).Times(1)

	w := httptest.NewRecorder()
	authzMux(t, mock, core.PermAll).ServeHTTP(w, doToken("test-token", http.MethodGet, "/admin/api/objects?prefix=", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestAuthz_GrantedOperationReachesTheStore is the positive case: a grant
// carrying the permission lets the request through to the object operation.
func TestAuthz_GrantedOperationReachesTheStore(t *testing.T) {
	t.Parallel()

	mock := storetest.NewMockObjectStore(gomock.NewController(t))
	mock.EXPECT().ListObjectsDelimited(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&core.ListDelimitedResult{}, nil).Times(1)

	w := httptest.NewRecorder()
	authzMux(t, mock, core.PermList).ServeHTTP(w, doToken(grantedToken, http.MethodGet, "/admin/api/objects?prefix=photos/", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}
