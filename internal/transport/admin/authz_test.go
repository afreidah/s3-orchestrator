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
// reaches grantedBucket with the given permissions and holds the given
// control-plane grants.
func registryGranting(t *testing.T, perms core.PermissionSet, admin map[core.Resource]core.PermissionSet) *auth.BucketRegistry {
	t.Helper()
	view := provisioning.View{
		Buckets: []provisioning.Bucket{{Name: grantedBucket, Source: provisioning.SourceStore}},
		Users: []provisioning.User{{
			ID:      "u1",
			Name:    "operator",
			Buckets: []string{grantedBucket},
			Grants:  map[string]core.PermissionSet{grantedBucket: perms},
			Admin:   admin,
			Source:  provisioning.SourceStore,
		}},
		Credentials: []provisioning.Credential{{
			AccessKeyID: "AKIATEST",
			UserID:      "u1",
			Token:       grantedToken,
			Source:      provisioning.SourceStore,
		}},
	}
	// The shared admin token resolves onto the root user rather than onto a
	// privileged flag, so a registry a token test runs against has to hold one.
	view.Users = append(view.Users, rootUser())
	registry, err := auth.NewBucketRegistry(&view)
	if err != nil {
		t.Fatalf("NewBucketRegistry: %v", err)
	}
	return registry
}

// authzMux builds a handler whose object operations read the given mock store
// and whose registry holds the granted credential, mounted on its own mux.
func authzMux(t *testing.T, mock core.ObjectStore, perms core.PermissionSet, admin map[core.Resource]core.PermissionSet) *http.ServeMux {
	t.Helper()
	cb := store.NewDatabaseBreaker(config.CircuitBreakerConfig{FailureThreshold: 3})
	var lv slog.LevelVar
	registry := registryGranting(t, perms, admin)
	h := &Handler{
		log:          slog.Default().With(logfmt.Component("admin")),
		dbHealthy:    cb.IsHealthy,
		objects:      objectsOver(t, mock),
		token:        "test-token",
		registry:     func() *auth.BucketRegistry { return registry },
		logLevel:     &lv,
		backendNames: func() []string { return []string{"b1", "b2"} },
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
	return serveAsAdmin(t, perms, nil, token, method, target)
}

// serveAsAdmin is serveAs with control-plane grants attached to the identity.
func serveAsAdmin(t *testing.T, perms core.PermissionSet, admin map[core.Resource]core.PermissionSet, token, method, target string) int {
	t.Helper()
	mock := storetest.NewMockObjectStore(gomock.NewController(t))
	w := httptest.NewRecorder()
	authzMux(t, mock, perms, admin).ServeHTTP(w, doToken(token, method, target, ""))
	return w.Code
}

// onInstance and onBackend name the resources the control-plane tests grant.
func onInstance(perms core.PermissionSet) map[core.Resource]core.PermissionSet {
	return map[core.Resource]core.PermissionSet{{Kind: core.ResourceInstance}: perms}
}

func onBackend(name string, perms core.PermissionSet) map[core.Resource]core.PermissionSet {
	return map[core.Resource]core.PermissionSet{{Kind: core.ResourceBackend, Name: name}: perms}
}

// routeFor finds one entry of the real route table, so a test asserting what a
// route authorizes is asserting what the table declares rather than a copy of
// it that can drift.
func routeFor(t *testing.T, method, pattern string) *route {
	t.Helper()
	h := &Handler{}
	rts := h.routes()
	for i := range rts {
		if rts[i].Method == method && rts[i].Pattern == pattern {
			return &rts[i]
		}
	}
	t.Fatalf("no route for %s %s", method, pattern)
	return nil
}

// decideAdmin reports whether one route's authorization lets the request
// through, without dispatching to the handler behind it. An allowed request
// would otherwise run an operation this bare handler holds no collaborators
// for, and the decision is what these tests are about.
func decideAdmin(t *testing.T, admin map[core.Resource]core.PermissionSet, rt *route, target string) bool {
	t.Helper()
	registry := registryGranting(t, 0, admin)
	h := &Handler{
		log:      slog.Default().With(logfmt.Component("admin")),
		token:    "test-token",
		registry: func() *auth.BucketRegistry { return registry },
	}
	r := doToken(grantedToken, rt.Method, target, "")
	who, ok := h.authenticate(r)
	if !ok {
		t.Fatal("the granted token did not authenticate")
	}
	return h.authorize(httptest.NewRecorder(), r, rt, who)
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

// TestAuthz_ControlPlaneRefusesBucketGrants pins that a credential carrying
// only bucket grants reaches no control-plane operation. Bucket grants say
// nothing about draining a backend, so a data-plane set must not read as one.
func TestAuthz_ControlPlaneRefusesBucketGrants(t *testing.T) {
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

// TestAuthz_ControlPlaneHoldsEachPermissionApart verifies the admin vocabulary
// is not one flag in disguise: a credential holding one control-plane
// permission is refused every operation the others name.
func TestAuthz_ControlPlaneHoldsEachPermissionApart(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		held   map[core.Resource]core.PermissionSet
		method string
		target string
	}{
		{"reader cannot rotate keys", onInstance(core.PermAdminRead), http.MethodPost, "/admin/api/rotate-encryption-key"},
		{"reader cannot provision", onInstance(core.PermAdminRead), http.MethodPost, "/admin/api/provisioning/users"},
		{"reader cannot read logs", onInstance(core.PermAdminRead), http.MethodGet, "/admin/api/logs"},
		{"reader cannot set the log level", onInstance(core.PermAdminRead), http.MethodPut, "/admin/api/log-level"},
		{"maintainer cannot decommission", onBackend("b1", core.PermAdminMaintain), http.MethodDelete, "/admin/api/backends/b1"},
		{"drainer cannot convert", onBackend("b1", core.PermAdminDrain), http.MethodPost, "/admin/api/encrypt-existing?backend=b1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := serveAsAdmin(t, 0, tc.held, grantedToken, tc.method, tc.target); got != http.StatusForbidden {
				t.Errorf("status = %d, want 403", got)
			}
		})
	}
}

// TestAuthz_BackendGrantDoesNotReachTheFleet is the rule that makes a
// backend-scoped grant worth issuing: a pass naming no backend runs against
// every one, so it needs the wildcard rather than a grant on one provider.
func TestAuthz_BackendGrantDoesNotReachTheFleet(t *testing.T) {
	t.Parallel()

	rt := routeFor(t, http.MethodPost, "/admin/api/encrypt-existing")
	for _, tc := range []struct {
		name   string
		target string
		want   bool
	}{
		{"the granted backend", "/admin/api/encrypt-existing?backend=b1", true},
		{"another backend", "/admin/api/encrypt-existing?backend=b2", false},
		{"the whole fleet", "/admin/api/encrypt-existing", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := decideAdmin(t, onBackend("b1", core.PermAdminConvert), rt, tc.target); got != tc.want {
				t.Errorf("authorized = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAuthz_BackendWildcardReachesEveryBackend is the other half: the wildcard
// is what an operator holds to run a pass over the fleet, and it answers for a
// named backend too.
func TestAuthz_BackendWildcardReachesEveryBackend(t *testing.T) {
	t.Parallel()

	admin := onBackend(core.ResourceWildcard, core.PermAdminConvert)
	rt := routeFor(t, http.MethodPost, "/admin/api/encrypt-existing")
	for _, target := range []string{
		"/admin/api/encrypt-existing",
		"/admin/api/encrypt-existing?backend=b1",
		"/admin/api/encrypt-existing?backend=b2",
	} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			if !decideAdmin(t, admin, rt, target) {
				t.Error("the wildcard grant did not authorize the request")
			}
		})
	}
}

// TestAuthz_EveryRouteDeclaresAPermission is what keeps the default closed. A
// route added without one authorizes nobody, which is a refusal an operator
// would otherwise report as a bug rather than as the missing declaration it is.
func TestAuthz_EveryRouteDeclaresAPermission(t *testing.T) {
	t.Parallel()

	h := &Handler{}
	for _, rt := range h.routes() {
		if rt.Perm == 0 {
			t.Errorf("%s %s declares no permission", rt.Method, rt.Pattern)
			continue
		}
		if err := core.ValidatePermissions(rt.kind(), rt.Perm); err != nil {
			t.Errorf("%s %s: %v", rt.Method, rt.Pattern, err)
		}
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
	authzMux(t, mock, core.PermAll, nil).ServeHTTP(w, doToken("test-token", http.MethodGet, "/admin/api/objects?prefix=", ""))

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
	authzMux(t, mock, core.PermList, nil).ServeHTTP(w, doToken(grantedToken, http.MethodGet, "/admin/api/objects?prefix=photos/", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// rootRegistry builds the registry a deployment carrying a shared admin token
// has: one root user holding every permission, which is what that token
// resolves onto now that it is no longer a privileged flag at the guard.
//
// The buckets the object tests name are declared, because root's bucket reach
// is expanded across what a deployment declares rather than being open-ended.
func rootRegistry(tb testing.TB) *auth.BucketRegistry {
	tb.Helper()
	v := provisioning.Merge(
		[]config.BucketConfig{{Name: "bucket"}, {Name: grantedBucket}},
		config.AuthConfig{LegacySharedToken: "test-token"},
		&provisioning.Snapshot{},
	)
	registry, err := auth.NewBucketRegistry(&v)
	if err != nil {
		tb.Fatalf("NewBucketRegistry: %v", err)
	}
	return registry
}

// rootUser is the identity the shared admin token resolves onto: every
// permission on every bucket, and every control-plane permission.
func rootUser() provisioning.User {
	return provisioning.User{
		ID:         provisioning.RootUserID,
		Name:       "root",
		AllBuckets: core.PermAll,
		Admin: map[core.Resource]core.PermissionSet{
			{Kind: core.ResourceInstance}:                             core.PermAdminAll,
			{Kind: core.ResourceBackend, Name: core.ResourceWildcard}: core.PermAdminAll,
		},
		Source: provisioning.SourceConfig,
	}
}
