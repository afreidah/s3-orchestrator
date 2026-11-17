// -------------------------------------------------------------------------------
// UI Handler - Authorization Tests
//
// Author: Alex Freidah
//
// The dashboard signs in against a credential and then acts on the fleet and
// its objects, so these drive the full mux with users holding narrower grants
// than root: a bucket-only credential, and an operator who may look but not
// act. They also revoke a key under a live session.
// -------------------------------------------------------------------------------

package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/provisioning"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
)

// The credentials the authorization tests sign in with.
const (
	appKey     = "AKIAUIAPP"
	appSecret  = "app-secret" //nolint:gosec // G101: test credential
	viewKey    = "AKIAUIVIEW"
	viewSecret = "view-secret" //nolint:gosec // G101: test credential
)

// grantedUsers is a snapshot holding an app credential with bucket access only
// and a viewer with admin-read and list on test-bucket.
func grantedUsers(withViewer bool) *provisioning.Snapshot {
	snap := &provisioning.Snapshot{
		Users: []core.User{{ID: "u-app", Name: "app"}, {ID: "u-view", Name: "viewer"}},
		Credentials: []core.Credential{
			{AccessKeyID: appKey, UserID: "u-app", Secret: appSecret},
		},
		Grants: []core.Grant{
			{UserID: "u-app", Resource: core.Resource{Kind: core.ResourceBucket, Name: "test-bucket"}, Permissions: core.PermAll},
			{UserID: "u-view", Resource: core.Resource{Kind: core.ResourceOrchestrator}, Permissions: core.PermAdminRead},
			{UserID: "u-view", Resource: core.Resource{Kind: core.ResourceBucket, Name: "test-bucket"}, Permissions: core.PermList},
		},
	}
	if withViewer {
		snap.Credentials = append(snap.Credentials, core.Credential{AccessKeyID: viewKey, UserID: "u-view", Secret: viewSecret})
	}
	return snap
}

// grantedHandler builds the test handler over a registry the test can swap, as
// a provisioning change does on a live instance.
func grantedHandler(t *testing.T) (*http.ServeMux, func(withViewer bool)) {
	t.Helper()
	h, mux, _ := newTestHandlerWithMock(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().ListObjects(gomock.Any(), "", gomock.Any(), gomock.Any(), gomock.Any()).
			Return(&core.ListObjectsResult{CommonPrefixes: []string{"hidden-bucket/"}}, nil).AnyTimes()
	})
	cfg := h.d.Config.Load()

	var current atomic.Pointer[auth.BucketRegistry]
	publish := func(withViewer bool) {
		view := provisioning.Merge(cfg.Buckets, cfg.Auth, grantedUsers(withViewer))
		br, err := auth.NewBucketRegistry(&view)
		if err != nil {
			t.Fatalf("NewBucketRegistry: %v", err)
		}
		current.Store(br)
	}
	publish(true)
	h.d.Registry = current.Load
	return mux, publish
}

// signIn posts a login and returns the response and the cookies it set.
func signIn(t *testing.T, mux *http.ServeMux, key, secret string) (*httptest.ResponseRecorder, []*http.Cookie) {
	t.Helper()
	form := url.Values{"access_key": {key}, "secret_key": {secret}}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/ui/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w, w.Result().Cookies()
}

// send issues a request carrying the session and, on a POST, the CSRF header.
func send(t *testing.T, mux *http.ServeMux, cookies []*http.Cookie, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
	for _, c := range cookies {
		req.AddCookie(c)
		if c.Name == csrfCookieName && method == http.MethodPost {
			req.Header.Set(csrfHeaderName, c.Value)
		}
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// TestLogin_RefusesCredentialWithoutAdminRead verifies a bucket-only credential
// cannot open a dashboard session.
func TestLogin_RefusesCredentialWithoutAdminRead(t *testing.T) {
	t.Parallel()
	mux, _ := grantedHandler(t)

	w, cookies := signIn(t, mux, appKey, appSecret)
	if w.Code != http.StatusForbidden {
		t.Errorf("login status = %d, want 403", w.Code)
	}
	for _, c := range cookies {
		if c.Name == sessionCookieName {
			t.Error("a session cookie was set for a credential without dashboard access")
		}
	}
}

// TestDashboard_ActionsFollowGrants verifies a viewer holding admin-read and
// bucket list can see the overview but cannot delete, upload, rebalance or
// start a backend pass.
func TestDashboard_ActionsFollowGrants(t *testing.T) {
	t.Parallel()
	mux, _ := grantedHandler(t)
	w, cookies := signIn(t, mux, viewKey, viewSecret)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("viewer login status = %d, want 303", w.Code)
	}

	if got := send(t, mux, cookies, http.MethodGet, "/ui/api/dashboard", "").Code; got != http.StatusOK {
		t.Errorf("GET /api/dashboard = %d, want 200", got)
	}

	for _, tc := range []struct {
		method, path, body string
	}{
		{http.MethodPost, "/ui/api/delete", `{"key":"test-bucket/a.txt"}`},
		{http.MethodPost, "/ui/api/delete-prefix", `{"prefix":"test-bucket/dir/"}`},
		{http.MethodGet, "/ui/api/download?key=test-bucket/a.txt", ""},
		{http.MethodPost, "/ui/api/rebalance", ""},
		{http.MethodPost, "/ui/api/scrub", ""},
		{http.MethodPost, "/ui/api/encrypt-existing", ""},
		{http.MethodPost, "/ui/api/sync", `{"backend":"b1","bucket":"test-bucket"}`},
		{http.MethodGet, "/ui/api/logs", ""},
	} {
		if got := send(t, mux, cookies, tc.method, tc.path, tc.body).Code; got != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403", tc.method, tc.path, got)
		}
	}
}

// TestTree_ListsOnlyListableBuckets verifies the tree root holds the buckets
// the user may list, an empty declared one included, and drops a bucket the
// ledger holds that they may not list.
func TestTree_ListsOnlyListableBuckets(t *testing.T) {
	t.Parallel()
	mux, _ := grantedHandler(t)
	_, cookies := signIn(t, mux, viewKey, viewSecret)

	w := send(t, mux, cookies, http.MethodGet, "/ui/api/tree", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/tree = %d, want 200", w.Code)
	}
	var got core.DirectoryListResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode tree: %v", err)
	}
	var names []string
	for _, e := range got.Entries {
		names = append(names, e.Name)
	}
	if len(names) != 1 || names[0] != "test-bucket/" {
		t.Errorf("tree root = %q, want only the viewer's listable test-bucket/", names)
	}
}

// TestRootEntries_PagedRootAddsOnlyCoveredBuckets verifies a root cut into
// pages adds each empty declared bucket on the one page whose range it falls
// in, in name order, and drops a held bucket the user may not list.
func TestRootEntries_PagedRootAddsOnlyCoveredBuckets(t *testing.T) {
	t.Parallel()
	list := map[string]core.PermissionSet{"a": core.PermList, "b": core.PermList, "c": core.PermList, "d": core.PermList}
	reg, err := auth.NewBucketRegistry(&provisioning.View{Users: []provisioning.User{{ID: "viewer", Name: "viewer", Grants: list}}})
	if err != nil {
		t.Fatalf("NewBucketRegistry: %v", err)
	}
	viewer, _ := reg.UserByID("viewer")
	ctx := withUser(context.Background(), viewer)
	declared := []string{"a", "b", "c", "d"}
	names := func(entries []core.DirEntry) []string {
		out := make([]string, len(entries))
		for i := range entries {
			out[i] = entries[i].Name
		}
		return out
	}

	first := &core.DirectoryListResult{
		Entries: []core.DirEntry{{Name: "a/", IsDir: true}, {Name: "c/", IsDir: true}},
		HasMore: true, NextCursor: "c/",
	}
	if got := names(rootEntries(ctx, first, "", declared)); !slices.Equal(got, []string{"a/", "b/", "c/"}) {
		t.Errorf("first page = %q, want a/ b/ c/", got)
	}
	last := &core.DirectoryListResult{Entries: []core.DirEntry{{Name: "x/", IsDir: true}}}
	if got := names(rootEntries(ctx, last, "c/", declared)); !slices.Equal(got, []string{"d/"}) {
		t.Errorf("last page = %q, want d/", got)
	}
}

// TestSession_EndsWhenKeyIsRevoked verifies removing the credential a session
// signed in with ends the session on its next request.
func TestSession_EndsWhenKeyIsRevoked(t *testing.T) {
	t.Parallel()
	mux, publish := grantedHandler(t)
	_, cookies := signIn(t, mux, viewKey, viewSecret)
	if got := send(t, mux, cookies, http.MethodGet, "/ui/api/dashboard", "").Code; got != http.StatusOK {
		t.Fatalf("before revocation: GET /api/dashboard = %d, want 200", got)
	}

	publish(false)

	if got := send(t, mux, cookies, http.MethodGet, "/ui/api/dashboard", "").Code; got != http.StatusUnauthorized {
		t.Errorf("after revocation: GET /api/dashboard = %d, want 401", got)
	}
}

// TestRoutes_EveryRouteDeclaresAuthorization verifies no dashboard route is
// mounted without a permission, which requireAuth would refuse outright.
func TestRoutes_EveryRouteDeclaresAuthorization(t *testing.T) {
	t.Parallel()
	for _, rt := range uiAPIRoutes {
		if rt.perm == 0 || rt.kind == "" {
			t.Errorf("route %s declares kind %q perm %v", rt.suffix, rt.kind, rt.perm)
		}
	}
}

// TestAuthorizeRoute_RefusesUndeclaredPermission verifies the fail-closed
// fallback for a route with no permission.
func TestAuthorizeRoute_RefusesUndeclaredPermission(t *testing.T) {
	t.Parallel()
	h, _ := newTestHandler(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/ui/api/x", nil)
	if h.authorizeRoute(w, r, &uiAPIRoute{suffix: "/api/x", kind: onOrchestrator}) {
		t.Error("a route declaring no permission was authorized")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}
