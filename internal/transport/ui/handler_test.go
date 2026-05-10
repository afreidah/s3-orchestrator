// -------------------------------------------------------------------------------
// UI Handler Tests
//
// Author: Alex Freidah
//
// Tests for the web dashboard HTTP handlers. Validates session authentication,
// login/logout flows, dashboard HTML rendering, JSON API endpoints for dashboard
// data and directory tree, delete/upload APIs, and static asset serving.
// -------------------------------------------------------------------------------

package ui

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/dashboard"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/testutil"
	"golang.org/x/crypto/bcrypt"
	// newTestHandler builds a Handler wired to mock data for testing.
)

// testAdminKey and related constants used by this package.
const (
	testAdminKey      = "test-admin"
	testAdminSecret   = "test-secret-key"
	testSessionSecret = "test-session-secret"
)

// newTestHandler constructs a new test handler.
func newTestHandler(t *testing.T) (*Handler, *http.ServeMux) {
	t.Helper()
	h, mux, _ := newTestHandlerWithMock(t)
	return h, mux
}

// newTestHandlerWithMock builds a Handler and also returns the underlying mock
// store so tests can configure per-test error/response behaviour.
func newTestHandlerWithMock(t *testing.T) (*Handler, *http.ServeMux, *testutil.MockStore) {
	t.Helper()

	mockStore := testutil.NewMockStore(t)
	mockStore.GetQuotaStatsResp = map[string]core.QuotaStat{
		"b1": {BackendName: "b1", BytesUsed: 500, BytesLimit: 1000},
	}
	mockStore.GetObjectCountsResp = map[string]int64{"b1": 42}
	mockStore.GetActiveMultipartResp = map[string]int64{"b1": 0}
	mockStore.GetUsageForPeriodResp = map[string]core.UsageStat{"b1": {APIRequests: 100}}
	mockStore.ListDirChildrenResp = &core.DirectoryListResult{
		Entries: []core.DirEntry{
			{Name: "bucket1/", IsDir: true, FileCount: 10, TotalSize: 4096},
		},
	}

	mgr := proxy.NewBackendManager(&proxy.BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{},
		Stores:          mockStore,
		Dashboard:       mockStore,
		Metrics:         mockStore,
		Order:           []string{"b1"},
		RoutingStrategy: config.RoutingPack,
	})
	proxytest.AttachWorkers(mgr, mockStore)
	t.Cleanup(mgr.Close)

	cfg := &config.Config{
		Buckets: []config.BucketConfig{
			{Name: "test-bucket"},
		},
		Backends: []config.BackendConfig{
			{Name: "b1", Endpoint: "https://s3.example.com", Bucket: "store",
				AccessKeyID: "AK", SecretAccessKey: "SK"},
		},
		RoutingStrategy: config.RoutingPack,
		Replication:     config.ReplicationConfig{Factor: 1},
		RateLimit:       config.RateLimitConfig{Enabled: false},
		UI: config.UIConfig{
			Enabled:       true,
			AdminKey:      testAdminKey,
			AdminSecret:   testAdminSecret,
			SessionSecret: testSessionSecret,
		},
	}

	h := New(&Deps{BackendOps: mgr, Objects: mgr.ObjectManager, Rebalancer: mgr.Rebalancer, OverRep: mgr.OverReplicationCleaner, DBHealthy: func() bool { return true }, Cfg: cfg, LogBuffer: telemetry.NewLogBuffer()})

	mux := http.NewServeMux()
	h.Register(mux, "/ui")

	return h, mux, mockStore
}

// getSessionCookie performs a login and returns the session cookie.
// loginCookies performs a login and returns the session and CSRF cookies.
// loginCookies login cookies.
// loginCookies login cookies.
func loginCookies(t *testing.T, h *Handler, mux *http.ServeMux) (session *http.Cookie, csrf *http.Cookie) {
	t.Helper()

	form := url.Values{
		"access_key": {testAdminKey},
		"secret_key": {testAdminSecret},
	}
	req := httptest.NewRequest(http.MethodPost, "/ui/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	for _, c := range w.Result().Cookies() {
		switch c.Name {
		case sessionCookieName:
			session = c
		case csrfCookieName:
			csrf = c
		}
	}
	if session == nil {
		t.Fatal("login did not set session cookie")
	}
	if csrf == nil {
		t.Fatal("login did not set CSRF cookie")
	}
	return session, csrf
}

// getSessionCookie performs a login and returns the session cookie.
func getSessionCookie(t *testing.T, h *Handler, mux *http.ServeMux) *http.Cookie {
	t.Helper()
	session, _ := loginCookies(t, h, mux)
	return session
}

// authedRequest creates a request with valid session and CSRF credentials.
// POST requests include the X-CSRF-Token header automatically.
func authedRequest(t *testing.T, h *Handler, mux *http.ServeMux, method, path string, body io.Reader) *http.Request {
	t.Helper()
	session, csrf := loginCookies(t, h, mux)
	req := httptest.NewRequest(method, path, body)
	req.AddCookie(session)
	req.AddCookie(csrf)
	if method == http.MethodPost {
		req.Header.Set(csrfHeaderName, csrf.Value)
	}
	return req
}

// -------------------------------------------------------------------------
// CSRF TESTS
// -------------------------------------------------------------------------

// TestCSRF_PostWithoutToken_Rejected verifies the csrf post without token rejected contract.
// Asserts that status = , want.
func TestCSRF_PostWithoutToken_Rejected(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)
	session := getSessionCookie(t, h, mux)

	// POST without CSRF token should be rejected
	req := httptest.NewRequest(http.MethodPost, "/ui/api/rebalance", nil)
	req.AddCookie(session)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

// TestCSRF_PostWithCookieButNoHeader_Rejected verifies the csrf post with cookie but no header rejected contract.
// Asserts that status = , want.
func TestCSRF_PostWithCookieButNoHeader_Rejected(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)
	session, csrf := loginCookies(t, h, mux)

	// Send CSRF cookie but no X-CSRF-Token header
	req := httptest.NewRequest(http.MethodPost, "/ui/api/rebalance", nil)
	req.AddCookie(session)
	req.AddCookie(csrf)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

// TestCSRF_PostWithWrongToken_Rejected verifies the csrf post with wrong token rejected contract.
// Asserts that status = , want.
func TestCSRF_PostWithWrongToken_Rejected(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)
	session, csrf := loginCookies(t, h, mux)

	req := httptest.NewRequest(http.MethodPost, "/ui/api/rebalance", nil)
	req.AddCookie(session)
	req.AddCookie(csrf)
	req.Header.Set(csrfHeaderName, "wrong-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

// TestCSRF_GetWithoutToken_Allowed verifies the csrf get without token allowed contract.
// Asserts that status = , want (GET should not require CSRF).
func TestCSRF_GetWithoutToken_Allowed(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)
	session := getSessionCookie(t, h, mux)

	// GET requests should not require CSRF token
	req := httptest.NewRequest(http.MethodGet, "/ui/api/dashboard", nil)
	req.AddCookie(session)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (GET should not require CSRF)", w.Code, http.StatusOK)
	}
}

// -------------------------------------------------------------------------
// HELPER TESTS
// -------------------------------------------------------------------------

// TestWriteJSONError verifies the write jsonerror contract.
// Asserts that status = , want.
func TestWriteJSONError(t *testing.T) {
	t.Parallel()
	w := httptest.NewRecorder()
	writeJSONError(w, http.StatusBadRequest, "something broke")

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if body := w.Body.String(); body != `{"error":"something broke"}` {
		t.Errorf("body = %q, want %q", body, `{"error":"something broke"}`)
	}
}

// -------------------------------------------------------------------------
// AUTH TESTS
// -------------------------------------------------------------------------

// TestDashboard_RequiresAuth verifies the dashboard requires auth contract.
// Asserts that status = , want 303 redirect.
func TestDashboard_RequiresAuth(t *testing.T) {
	t.Parallel()
	_, mux := newTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/ui/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 redirect", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc != "/ui/login" {
		t.Errorf("Location = %q, want /ui/login", loc)
	}
}

// TestAPIDashboard_RequiresAuth verifies the apidashboard requires auth contract.
// Asserts that status = , want 401.
func TestAPIDashboard_RequiresAuth(t *testing.T) {
	t.Parallel()
	_, mux := newTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/ui/api/dashboard", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Result().StatusCode)
	}
}

// TestLogin_ValidCredentials verifies the login valid credentials contract.
// Asserts that status = , want 303.
func TestLogin_ValidCredentials(t *testing.T) {
	t.Parallel()
	_, mux := newTestHandler(t)

	form := url.Values{
		"access_key": {testAdminKey},
		"secret_key": {testAdminSecret},
	}
	req := httptest.NewRequest(http.MethodPost, "/ui/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	if resp.Header.Get("Location") != "/ui/" {
		t.Errorf("Location = %q, want /ui/", resp.Header.Get("Location"))
	}

	var found bool
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName {
			found = true
			if !c.HttpOnly {
				t.Error("session cookie should be HttpOnly")
			}
			if c.SameSite != http.SameSiteStrictMode {
				t.Error("session cookie should be SameSite=Strict")
			}
		}
	}
	if !found {
		t.Error("login response missing session cookie")
	}
}

// TestLogin_InvalidCredentials verifies the login invalid credentials contract.
// Asserts that status = , want 401.
func TestLogin_InvalidCredentials(t *testing.T) {
	t.Parallel()
	_, mux := newTestHandler(t)

	form := url.Values{
		"access_key": {"wrong"},
		"secret_key": {"wrong"},
	}
	req := httptest.NewRequest(http.MethodPost, "/ui/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Invalid credentials") {
		t.Error("response should contain error message")
	}
}

// TestLogin_GET_ShowsForm verifies the login get shows form contract.
// Asserts that status = , want 200.
func TestLogin_GET_ShowsForm(t *testing.T) {
	t.Parallel()
	_, mux := newTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/ui/login", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Sign In") {
		t.Error("login page should contain Sign In button")
	}
}

// TestLogin_GET_RedirectsWhenAuthenticated verifies the login get redirects when authenticated contract.
// Asserts that status = , want 303.
func TestLogin_GET_RedirectsWhenAuthenticated(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	cookie := getSessionCookie(t, h, mux)
	req := httptest.NewRequest(http.MethodGet, "/ui/login", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	if resp.Header.Get("Location") != "/ui/" {
		t.Errorf("Location = %q, want /ui/", resp.Header.Get("Location"))
	}
}

// TestLogout_ClearsCookie verifies the logout clears cookie contract.
// Asserts that status = , want 303.
func TestLogout_ClearsCookie(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	cookie := getSessionCookie(t, h, mux)
	req := httptest.NewRequest(http.MethodGet, "/ui/logout", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName && c.MaxAge < 0 {
			return // cookie cleared
		}
	}
	t.Error("logout should clear session cookie")
}

// TestCheckSecret_Plaintext verifies the check secret plaintext behaviour described by the test name.
func TestCheckSecret_Plaintext(t *testing.T) {
	t.Parallel()
	if !checkSecret("mysecret", "mysecret") {
		t.Error("identical plaintext should match")
	}
	if checkSecret("mysecret", "wrong") {
		t.Error("different plaintext should not match")
	}
}

// TestCheckSecret_Bcrypt verifies the check secret bcrypt path by exercising bcrypt.GenerateFromPassword.
func TestCheckSecret_Bcrypt(t *testing.T) {
	t.Parallel()
	hash, err := bcrypt.GenerateFromPassword([]byte("bcrypt-pass"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}

	if !checkSecret(string(hash), "bcrypt-pass") {
		t.Error("correct password should match bcrypt hash")
	}
	if checkSecret(string(hash), "wrong") {
		t.Error("wrong password should not match bcrypt hash")
	}
}

// TestLogin_BcryptSecret verifies the login bcrypt secret contract.
// Asserts that bcrypt login: status = , want 303.
func TestLogin_BcryptSecret(t *testing.T) {
	t.Parallel()
	hash, err := bcrypt.GenerateFromPassword([]byte(testAdminSecret), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}

	mockStore := testutil.NewMockStore(t)
	mockStore.GetQuotaStatsResp = map[string]core.QuotaStat{}
	mockStore.GetObjectCountsResp = map[string]int64{}
	mockStore.GetActiveMultipartResp = map[string]int64{}
	mockStore.GetUsageForPeriodResp = map[string]core.UsageStat{}
	mockStore.ListDirChildrenResp = &core.DirectoryListResult{}
	mgr := proxy.NewBackendManager(&proxy.BackendManagerConfig{
		Backends:  map[string]backend.ObjectBackend{},
		Stores:    mockStore,
		Dashboard: mockStore,
		Metrics:   mockStore,
		Order:     []string{},
	})
	proxytest.AttachWorkers(mgr, mockStore)
	t.Cleanup(mgr.Close)

	cfg := &config.Config{
		Buckets:  []config.BucketConfig{{Name: "b"}},
		Backends: []config.BackendConfig{{Name: "b1", Endpoint: "e", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s"}},
		UI: config.UIConfig{
			Enabled:       true,
			AdminKey:      testAdminKey,
			AdminSecret:   string(hash),
			SessionSecret: testSessionSecret,
		},
	}

	h := New(&Deps{BackendOps: mgr, Objects: mgr.ObjectManager, Rebalancer: mgr.Rebalancer, OverRep: mgr.OverReplicationCleaner, DBHealthy: func() bool { return true }, Cfg: cfg, LogBuffer: telemetry.NewLogBuffer()})
	mux := http.NewServeMux()
	h.Register(mux, "/ui")

	form := url.Values{
		"access_key": {testAdminKey},
		"secret_key": {testAdminSecret},
	}
	req := httptest.NewRequest(http.MethodPost, "/ui/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusSeeOther {
		t.Fatalf("bcrypt login: status = %d, want 303", w.Result().StatusCode)
	}
}

// TestDeriveSessionKey_Deterministic verifies the derive session key deterministic path by exercising bytes.Equal.
func TestDeriveSessionKey_Deterministic(t *testing.T) {
	t.Parallel()
	ui := config.UIConfig{SessionSecret: "shared-secret"}
	key1 := deriveSessionKey(&ui)
	key2 := deriveSessionKey(&ui)

	if !bytes.Equal(key1, key2) {
		t.Error("same config should produce identical session keys")
	}
}

// TestDeriveSessionKey_DifferentSecretsDifferentKeys verifies the derive session key different secrets different keys path by exercising bytes.Equal.
func TestDeriveSessionKey_DifferentSecretsDifferentKeys(t *testing.T) {
	t.Parallel()
	ui1 := config.UIConfig{SessionSecret: "secret-one"}
	ui2 := config.UIConfig{SessionSecret: "secret-two"}

	key1 := deriveSessionKey(&ui1)
	key2 := deriveSessionKey(&ui2)

	if bytes.Equal(key1, key2) {
		t.Error("different session_secret values should produce different keys")
	}
}

// TestCrossInstanceSession verifies the cross instance session contract.
// Asserts that cross-instance session: status = , want 200.
func TestCrossInstanceSession(t *testing.T) {
	t.Parallel()
	// Two handlers with the same config should accept each other's sessions.
	mockStore := testutil.NewMockStore(t)
	mockStore.GetQuotaStatsResp = map[string]core.QuotaStat{}
	mockStore.GetObjectCountsResp = map[string]int64{}
	mockStore.GetActiveMultipartResp = map[string]int64{}
	mockStore.GetUsageForPeriodResp = map[string]core.UsageStat{}
	mockStore.ListDirChildrenResp = &core.DirectoryListResult{}
	mgr := proxy.NewBackendManager(&proxy.BackendManagerConfig{
		Backends:  map[string]backend.ObjectBackend{},
		Stores:    mockStore,
		Dashboard: mockStore,
		Metrics:   mockStore,
		Order:     []string{},
	})
	proxytest.AttachWorkers(mgr, mockStore)
	t.Cleanup(mgr.Close)

	cfg := &config.Config{
		Buckets:  []config.BucketConfig{{Name: "b"}},
		Backends: []config.BackendConfig{{Name: "b1", Endpoint: "e", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s"}},
		UI: config.UIConfig{
			Enabled:     true,
			AdminKey:    testAdminKey,
			AdminSecret: testAdminSecret,
		},
	}

	h1 := New(&Deps{BackendOps: mgr, Objects: mgr.ObjectManager, Rebalancer: mgr.Rebalancer, OverRep: mgr.OverReplicationCleaner, DBHealthy: func() bool { return true }, Cfg: cfg, LogBuffer: telemetry.NewLogBuffer()})
	h2 := New(&Deps{BackendOps: mgr, Objects: mgr.ObjectManager, Rebalancer: mgr.Rebalancer, OverRep: mgr.OverReplicationCleaner, DBHealthy: func() bool { return true }, Cfg: cfg, LogBuffer: telemetry.NewLogBuffer()})
	mux1 := http.NewServeMux()
	mux2 := http.NewServeMux()
	h1.Register(mux1, "/ui")
	h2.Register(mux2, "/ui")

	// Login on instance 1.
	cookie := getSessionCookie(t, h1, mux1)

	// Use that session on instance 2.
	req := httptest.NewRequest(http.MethodGet, "/ui/api/dashboard", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	mux2.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("cross-instance session: status = %d, want 200", w.Result().StatusCode)
	}
}

// TestStaticAssets_NoAuthRequired verifies the static assets no auth required contract.
// Asserts that status = , want 200 (no auth required for static assets).
func TestStaticAssets_NoAuthRequired(t *testing.T) {
	t.Parallel()
	_, mux := newTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/ui/static/style.css", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no auth required for static assets)", resp.StatusCode)
	}
}

// -------------------------------------------------------------------------
// DASHBOARD TESTS (AUTHENTICATED)
// -------------------------------------------------------------------------

// TestDashboard_Returns200HTML verifies the dashboard returns200 html contract.
// Asserts that status = , want 200.
func TestDashboard_Returns200HTML(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodGet, "/ui/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "S3 Orchestrator") {
		t.Error("response body missing expected title")
	}
	if !strings.Contains(string(body), "Storage Summary") {
		t.Error("response body missing Storage Summary section")
	}
	if !strings.Contains(string(body), "Backends") {
		t.Error("response body missing Backends section")
	}
	if !strings.Contains(string(body), "Logout") {
		t.Error("response body missing Logout link")
	}
}

// TestAPIDashboard_ReturnsJSON verifies the apidashboard returns json contract.
// Asserts that status = , want 200.
func TestAPIDashboard_ReturnsJSON(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/dashboard", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var data dashboard.Data
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}
	if len(data.QuotaStats) == 0 {
		t.Error("expected non-empty QuotaStats")
	}
}

// TestTreeAPI_ReturnsJSON verifies the tree api returns json contract.
// Asserts that status = , want 200.
func TestTreeAPI_ReturnsJSON(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/tree?prefix=", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var result core.DirectoryListResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}
	if len(result.Entries) == 0 {
		t.Error("expected entries in tree response")
	}
}

// TestSecurityHeaders_PresentOnAllEndpoints verifies the security headers present on all endpoints contract.
// Asserts that = , want.
func TestSecurityHeaders_PresentOnAllEndpoints(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	// Login page (no auth needed) and authenticated endpoints
	endpoints := []struct {
		path   string
		authed bool
	}{
		{"/ui/login", false},
		{"/ui/", true},
		{"/ui/api/dashboard", true},
		{"/ui/api/tree?prefix=", true},
	}
	for _, ep := range endpoints {
		t.Run(ep.path, func(t *testing.T) {
			var req *http.Request
			if ep.authed {
				req = authedRequest(t, h, mux, http.MethodGet, ep.path, nil)
			} else {
				req = httptest.NewRequest(http.MethodGet, ep.path, nil)
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			resp := w.Result()
			checks := map[string]string{
				"X-Frame-Options":         "DENY",
				"X-Content-Type-Options":  "nosniff",
				"Referrer-Policy":         "strict-origin-when-cross-origin",
				"Content-Security-Policy": "default-src 'self'; style-src 'self' 'unsafe-inline'",
			}
			for header, want := range checks {
				got := resp.Header.Get(header)
				if got != want {
					t.Errorf("%s = %q, want %q", header, got, want)
				}
			}
		})
	}
}

// TestUpdateConfig_ReflectsInDashboard verifies the update config reflects in dashboard path by exercising h.UpdateConfig, httptest.NewRecorder, mux.ServeHTTP.
func TestUpdateConfig_ReflectsInDashboard(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	// Update config with a different routing strategy
	newCfg := &config.Config{
		Buckets: []config.BucketConfig{
			{Name: "updated-bucket"},
		},
		RoutingStrategy: config.RoutingSpread,
		Replication:     config.ReplicationConfig{Factor: 2},
		RateLimit:       config.RateLimitConfig{Enabled: true},
	}
	h.UpdateConfig(newCfg)

	req := authedRequest(t, h, mux, http.MethodGet, "/ui/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body, _ := io.ReadAll(w.Result().Body)
	html := string(body)

	if !strings.Contains(html, "spread") {
		t.Error("dashboard should reflect updated routing strategy 'spread'")
	}
	if !strings.Contains(html, "updated-bucket") {
		t.Error("dashboard should reflect updated bucket name")
	}
}

// -------------------------------------------------------------------------
// DELETE / UPLOAD AUTH GATING
// -------------------------------------------------------------------------

// TestAPIDelete_RequiresAuth verifies the apidelete requires auth contract.
// Asserts that status = , want 401.
func TestAPIDelete_RequiresAuth(t *testing.T) {
	t.Parallel()
	_, mux := newTestHandler(t)

	req := httptest.NewRequest(http.MethodPost, "/ui/api/delete", strings.NewReader(`{"key":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Result().StatusCode)
	}
}

// TestAPIUpload_RequiresAuth verifies the apiupload requires auth contract.
// Asserts that status = , want 401.
func TestAPIUpload_RequiresAuth(t *testing.T) {
	t.Parallel()
	_, mux := newTestHandler(t)

	req := httptest.NewRequest(http.MethodPost, "/ui/api/upload", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Result().StatusCode)
	}
}

// -------------------------------------------------------------------------
// DELETE API TESTS
// -------------------------------------------------------------------------

// TestAPIDelete_WrongMethod verifies the apidelete wrong method contract.
// Asserts that status = , want 405.
func TestAPIDelete_WrongMethod(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/delete", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Result().StatusCode)
	}
}

// TestAPIDelete_BadJSON verifies the apidelete bad json contract.
// Asserts that status = , want 400.
func TestAPIDelete_BadJSON(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodPost, "/ui/api/delete", strings.NewReader("{bad"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Result().StatusCode)
	}
}

// TestAPIDelete_EmptyKey verifies the apidelete empty key contract.
// Asserts that status = , want 400.
func TestAPIDelete_EmptyKey(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodPost, "/ui/api/delete", strings.NewReader(`{"key":""}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Result().StatusCode)
	}
}

// TestAPIDelete_Success verifies the apidelete success contract.
// Asserts that status = , want 200; body =.
func TestAPIDelete_Success(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodPost, "/ui/api/delete",
		strings.NewReader(`{"key":"test-bucket/file.txt"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body = %s", resp.StatusCode, body)
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if result["ok"] != true {
		t.Errorf("expected ok=true, got %v", result)
	}
}

// TestAPIDelete_ManagerError verifies the apidelete manager error contract.
// Asserts that status = , want 500.
func TestAPIDelete_ManagerError(t *testing.T) {
	t.Parallel()
	h, mux, mock := newTestHandlerWithMock(t)
	mock.DeleteObjectErr = errors.New("db down")

	req := authedRequest(t, h, mux, http.MethodPost, "/ui/api/delete",
		strings.NewReader(`{"key":"test-bucket/file.txt"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Result().StatusCode)
	}
}

// -------------------------------------------------------------------------
// DELETE PREFIX API TESTS
// -------------------------------------------------------------------------

// TestAPIDeletePrefix_RequiresAuth verifies the apidelete prefix requires auth contract.
// Asserts that status = , want 401.
func TestAPIDeletePrefix_RequiresAuth(t *testing.T) {
	t.Parallel()
	_, mux := newTestHandler(t)

	req := httptest.NewRequest(http.MethodPost, "/ui/api/delete-prefix",
		strings.NewReader(`{"prefix":"test-bucket/"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Result().StatusCode)
	}
}

// TestAPIDeletePrefix_WrongMethod verifies the apidelete prefix wrong method contract.
// Asserts that status = , want 405.
func TestAPIDeletePrefix_WrongMethod(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/delete-prefix", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Result().StatusCode)
	}
}

// TestAPIDeletePrefix_BadJSON verifies the apidelete prefix bad json contract.
// Asserts that status = , want 400.
func TestAPIDeletePrefix_BadJSON(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodPost, "/ui/api/delete-prefix", strings.NewReader("{bad"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Result().StatusCode)
	}
}

// TestAPIDeletePrefix_EmptyPrefix verifies the apidelete prefix empty prefix contract.
// Asserts that status = , want 400.
func TestAPIDeletePrefix_EmptyPrefix(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodPost, "/ui/api/delete-prefix",
		strings.NewReader(`{"prefix":""}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Result().StatusCode)
	}
}

// TestAPIDeletePrefix_Success verifies the apidelete prefix success contract.
// Asserts that status = , want 200; body =.
func TestAPIDeletePrefix_Success(t *testing.T) {
	t.Parallel()
	h, mux, mock := newTestHandlerWithMock(t)
	mock.ListObjectsResp = &core.ListObjectsResult{
		Objects: []core.ObjectLocation{
			{ObjectKey: "test-bucket/a.txt", BackendName: "b1", SizeBytes: 100},
			{ObjectKey: "test-bucket/b.txt", BackendName: "b1", SizeBytes: 200},
		},
		IsTruncated: false,
	}

	req := authedRequest(t, h, mux, http.MethodPost, "/ui/api/delete-prefix",
		strings.NewReader(`{"prefix":"test-bucket/"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body = %s", resp.StatusCode, body)
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if result["ok"] != true {
		t.Errorf("expected ok=true, got %v", result)
	}
	if int(result["deleted"].(float64)) != 2 {
		t.Errorf("expected deleted=2, got %v", result["deleted"])
	}
}

// TestAPIDeletePrefix_EmptyResult verifies the apidelete prefix empty result contract.
// Asserts that status = , want 200; body =.
func TestAPIDeletePrefix_EmptyResult(t *testing.T) {
	t.Parallel()
	h, mux, mock := newTestHandlerWithMock(t)
	mock.ListObjectsResp = &core.ListObjectsResult{
		Objects:     nil,
		IsTruncated: false,
	}

	req := authedRequest(t, h, mux, http.MethodPost, "/ui/api/delete-prefix",
		strings.NewReader(`{"prefix":"empty-prefix/"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body = %s", resp.StatusCode, body)
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if result["ok"] != true {
		t.Errorf("expected ok=true, got %v", result)
	}
	if int(result["deleted"].(float64)) != 0 {
		t.Errorf("expected deleted=0, got %v", result["deleted"])
	}
}

// TestAPIDeletePrefix_ListObjectsError verifies the apidelete prefix list objects error contract.
// Asserts that status = , want 500.
func TestAPIDeletePrefix_ListObjectsError(t *testing.T) {
	t.Parallel()
	h, mux, mock := newTestHandlerWithMock(t)
	mock.ListObjectsErr = errors.New("db down")

	req := authedRequest(t, h, mux, http.MethodPost, "/ui/api/delete-prefix",
		strings.NewReader(`{"prefix":"test-bucket/"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Result().StatusCode)
	}
}

// TestAPIDeletePrefix_DeleteError verifies the apidelete prefix delete error contract.
// Asserts that status = , want 500.
func TestAPIDeletePrefix_DeleteError(t *testing.T) {
	t.Parallel()
	h, mux, mock := newTestHandlerWithMock(t)
	mock.ListObjectsResp = &core.ListObjectsResult{
		Objects: []core.ObjectLocation{
			{ObjectKey: "test-bucket/a.txt", BackendName: "b1", SizeBytes: 100},
		},
		IsTruncated: false,
	}
	mock.DeleteObjectsBatchErr = errors.New("delete failed")

	req := authedRequest(t, h, mux, http.MethodPost, "/ui/api/delete-prefix",
		strings.NewReader(`{"prefix":"test-bucket/"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if result["error"] == nil {
		t.Error("expected error field in response")
	}
}

// -------------------------------------------------------------------------
// UPLOAD API TESTS
// -------------------------------------------------------------------------

// multipartForm builds a multipart/form-data request body with a key field and file.
func multipartForm(t *testing.T, key, filename string, fileContent []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	if key != "" {
		if err := w.WriteField("key", key); err != nil {
			t.Fatal(err)
		}
	}
	if filename != "" {
		part, err := w.CreateFormFile("file", filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(fileContent); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, w.FormDataContentType()
}

// TestAPIUpload_WrongMethod verifies the apiupload wrong method contract.
// Asserts that status = , want 405.
func TestAPIUpload_WrongMethod(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/upload", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Result().StatusCode)
	}
}

// TestAPIUpload_MissingKey verifies the apiupload missing key contract.
// Asserts that status = , want 400.
func TestAPIUpload_MissingKey(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	body, ct := multipartForm(t, "", "test.txt", []byte("hello"))
	req := authedRequest(t, h, mux, http.MethodPost, "/ui/api/upload", body)
	req.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Result().StatusCode)
	}
}

// TestAPIUpload_InvalidBucket verifies the apiupload invalid bucket contract.
// Asserts that status = , want 400.
func TestAPIUpload_InvalidBucket(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	body, ct := multipartForm(t, "wrong-bucket/file.txt", "file.txt", []byte("hello"))
	req := authedRequest(t, h, mux, http.MethodPost, "/ui/api/upload", body)
	req.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Result().StatusCode)
	}
	respBody, _ := io.ReadAll(w.Result().Body)
	if !strings.Contains(string(respBody), "configured bucket") {
		t.Errorf("expected bucket validation error, got: %s", respBody)
	}
}

// TestAPIUpload_MissingFile verifies the apiupload missing file contract.
// Asserts that status = , want 400.
func TestAPIUpload_MissingFile(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	// Form with key but no file field.
	body, ct := multipartForm(t, "test-bucket/file.txt", "", nil)
	req := authedRequest(t, h, mux, http.MethodPost, "/ui/api/upload", body)
	req.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Result().StatusCode)
	}
}

// TestAPIUpload_PutObjectError verifies the apiupload put object error contract.
// Asserts that status = , want 500.
func TestAPIUpload_PutObjectError(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	// Valid form, but PutObject will fail because no real backend is wired up.
	body, ct := multipartForm(t, "test-bucket/file.txt", "file.txt", []byte("data"))
	req := authedRequest(t, h, mux, http.MethodPost, "/ui/api/upload", body)
	req.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Result().StatusCode)
	}
}

// -------------------------------------------------------------------------
// REBALANCE API TESTS
// -------------------------------------------------------------------------

// TestAPIRebalance_RequiresAuth verifies the apirebalance requires auth contract.
// Asserts that status = , want 401.
func TestAPIRebalance_RequiresAuth(t *testing.T) {
	t.Parallel()
	_, mux := newTestHandler(t)

	req := httptest.NewRequest(http.MethodPost, "/ui/api/rebalance", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Result().StatusCode)
	}
}

// TestAPIRebalance_WrongMethod verifies the apirebalance wrong method contract.
// Asserts that status = , want 405.
func TestAPIRebalance_WrongMethod(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/rebalance", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Result().StatusCode)
	}
}

// TestAPIRebalance_Success verifies the apirebalance success contract.
// Asserts that status = , want 202; body =.
func TestAPIRebalance_Success(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodPost, "/ui/api/rebalance", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 202; body = %s", resp.StatusCode, body)
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if result["status"] != "started" {
		t.Errorf("expected status=started, got %v", result)
	}
}

// TestAPIRebalance_AlreadyRunning verifies the apirebalance already running contract.
// Asserts that second trigger: status = , want 409.
func TestAPIRebalance_AlreadyRunning(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	// Simulate a rebalance already in progress by marking the operation as
	// running directly. The async goroutine completes too fast for a
	// second HTTP request to race against it reliably.
	h.asyncOps.TryStart("rebalance")

	// Should conflict while the operation is marked as running
	req := authedRequest(t, h, mux, http.MethodPost, "/ui/api/rebalance", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusConflict {
		t.Fatalf("second trigger: status = %d, want 409", w.Result().StatusCode)
	}
}

// TestAPIRebalance_StatusPolling verifies the apirebalance status polling contract.
// Asserts that expected status=idle, got.
func TestAPIRebalance_StatusPolling(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	// Status before any run should be idle
	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/rebalance/status", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	var result map[string]any
	_ = json.NewDecoder(w.Body).Decode(&result)
	if result["status"] != "idle" {
		t.Errorf("expected status=idle, got %v", result)
	}
}

// TestAPIRebalance_ManagerError verifies the apirebalance manager error contract.
// Asserts that status = , want 202.
func TestAPIRebalance_ManagerError(t *testing.T) {
	t.Parallel()
	h, mux, mock := newTestHandlerWithMock(t)
	mock.GetQuotaStatsErr = errors.New("db down")

	req := authedRequest(t, h, mux, http.MethodPost, "/ui/api/rebalance", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// Async trigger still returns 202  -  errors surface via status endpoint
	if w.Result().StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Result().StatusCode)
	}

	// Wait briefly for background goroutine to complete
	time.Sleep(100 * time.Millisecond)

	// Poll status to see the error
	req2 := authedRequest(t, h, mux, http.MethodGet, "/ui/api/rebalance/status", nil)
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)

	var result map[string]any
	_ = json.NewDecoder(w2.Body).Decode(&result)
	if result["status"] != "error" {
		t.Errorf("expected status=error, got %v", result)
	}
}

// -------------------------------------------------------------------------
// SYNC API TESTS
// -------------------------------------------------------------------------

// TestAPISync_RequiresAuth verifies the apisync requires auth contract.
// Asserts that status = , want 401.
func TestAPISync_RequiresAuth(t *testing.T) {
	t.Parallel()
	_, mux := newTestHandler(t)

	req := httptest.NewRequest(http.MethodPost, "/ui/api/sync",
		strings.NewReader(`{"backend":"b1","bucket":"test-bucket"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Result().StatusCode)
	}
}

// TestAPISync_WrongMethod verifies the apisync wrong method contract.
// Asserts that status = , want 405.
func TestAPISync_WrongMethod(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/sync", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Result().StatusCode)
	}
}

// TestAPISync_BadJSON verifies the apisync bad json contract.
// Asserts that status = , want 400.
func TestAPISync_BadJSON(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodPost, "/ui/api/sync", strings.NewReader("{bad"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Result().StatusCode)
	}
}

// TestAPISync_EmptyFields verifies the apisync empty fields contract.
// Asserts that status = , want 400.
func TestAPISync_EmptyFields(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodPost, "/ui/api/sync",
		strings.NewReader(`{"backend":"","bucket":""}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Result().StatusCode)
	}
}

// TestAPISync_UnknownBackend verifies the apisync unknown backend contract.
// Asserts that status = , want 400.
func TestAPISync_UnknownBackend(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodPost, "/ui/api/sync",
		strings.NewReader(`{"backend":"nonexistent","bucket":"test-bucket"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "invalid backend or bucket") {
		t.Errorf("expected invalid backend or bucket error, got: %s", body)
	}
}

// TestAPISync_UnknownBucket verifies the apisync unknown bucket contract.
// Asserts that status = , want 400.
func TestAPISync_UnknownBucket(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodPost, "/ui/api/sync",
		strings.NewReader(`{"backend":"b1","bucket":"nonexistent"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "invalid backend or bucket") {
		t.Errorf("expected invalid backend or bucket error, got: %s", body)
	}
}

// TestAPISync_ManagerError verifies the apisync manager error contract.
// Asserts that status = , want 500.
func TestAPISync_ManagerError(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	// SyncBackend fails because the mock store isn't a concrete *Store.
	req := authedRequest(t, h, mux, http.MethodPost, "/ui/api/sync",
		strings.NewReader(`{"backend":"b1","bucket":"test-bucket"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Result().StatusCode)
	}
}

// -------------------------------------------------------------------------
// ERROR PATH TESTS
// -------------------------------------------------------------------------

// TestLogin_UnsupportedMethod verifies the login unsupported method contract.
// Asserts that status = , want 405.
func TestLogin_UnsupportedMethod(t *testing.T) {
	t.Parallel()
	_, mux := newTestHandler(t)

	req := httptest.NewRequest(http.MethodPut, "/ui/login", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Result().StatusCode)
	}
}

// TestDashboard_DataError verifies the dashboard data error contract.
// Asserts that status = , want 500.
func TestDashboard_DataError(t *testing.T) {
	t.Parallel()
	h, mux, mock := newTestHandlerWithMock(t)
	mock.GetQuotaStatsErr = errors.New("db down")

	req := authedRequest(t, h, mux, http.MethodGet, "/ui/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Result().StatusCode)
	}
}

// TestAPIDashboard_DataError verifies the apidashboard data error contract.
// Asserts that status = , want 500.
func TestAPIDashboard_DataError(t *testing.T) {
	t.Parallel()
	h, mux, mock := newTestHandlerWithMock(t)
	mock.GetQuotaStatsErr = errors.New("db down")

	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/dashboard", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Result().StatusCode)
	}
}

// TestTreeAPI_WithMaxKeys verifies the tree api with max keys contract.
// Asserts that status = , want 200.
func TestTreeAPI_WithMaxKeys(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/tree?prefix=&maxKeys=50", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Result().StatusCode)
	}
}

// TestTreeAPI_InvalidBucketPrefix verifies the tree api invalid bucket prefix contract.
// Asserts that status = , want 400.
func TestTreeAPI_InvalidBucketPrefix(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/tree?prefix=no-such-bucket/dir", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Result().StatusCode)
	}
}

// TestTreeAPI_DataError verifies the tree api data error contract.
// Asserts that status = , want 500.
func TestTreeAPI_DataError(t *testing.T) {
	t.Parallel()
	h, mux, mock := newTestHandlerWithMock(t)
	mock.ListDirChildrenErr = errors.New("db down")

	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/tree?prefix=", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Result().StatusCode)
	}
}

// -------------------------------------------------------------------------
// LOGS API TESTS
// -------------------------------------------------------------------------

// TestAPILogs_RequiresAuth verifies the apilogs requires auth contract.
// Asserts that status = , want 401.
func TestAPILogs_RequiresAuth(t *testing.T) {
	t.Parallel()
	_, mux := newTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/ui/api/logs", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Result().StatusCode)
	}
}

// TestAPILogs_ReturnsJSON verifies the apilogs returns json contract.
// Asserts that status = , want 200.
func TestAPILogs_ReturnsJSON(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/logs", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var lr logsResponse
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}
}

// TestAPILogs_LevelFilter verifies the apilogs level filter contract.
// Asserts that status = , want 200.
func TestAPILogs_LevelFilter(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	// Add known entries to the handler's log buffer.
	h.logBuffer.Add(telemetry.LogEntry{Level: "INFO", Message: "info msg"})
	h.logBuffer.Add(telemetry.LogEntry{Level: "ERROR", Message: "error msg"})

	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/logs?level=ERROR", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var lr logsResponse
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}
	if len(lr.Entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(lr.Entries))
	}
	if lr.Entries[0].Message != "error msg" {
		t.Errorf("message = %q, want 'error msg'", lr.Entries[0].Message)
	}
}

// TestAPILogs_AllLevelFilters verifies the apilogs all level filters contract.
// Asserts that status = , want 200.
func TestAPILogs_AllLevelFilters(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	h.logBuffer.Add(telemetry.LogEntry{Level: "DEBUG", Message: "d"})
	h.logBuffer.Add(telemetry.LogEntry{Level: "INFO", Message: "i"})
	h.logBuffer.Add(telemetry.LogEntry{Level: "WARN", Message: "w"})
	h.logBuffer.Add(telemetry.LogEntry{Level: "ERROR", Message: "e"})

	tests := []struct {
		level string
		want  int
	}{
		{"DEBUG", 4},
		{"INFO", 3},
		{"WARN", 2},
		{"ERROR", 1},
	}
	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/logs?level="+tt.level, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Result().StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Result().StatusCode)
			}
			var lr logsResponse
			if err := json.NewDecoder(w.Result().Body).Decode(&lr); err != nil {
				t.Fatalf("failed to decode: %v", err)
			}
			if len(lr.Entries) != tt.want {
				t.Errorf("level=%s: got %d entries, want %d", tt.level, len(lr.Entries), tt.want)
			}
		})
	}
}

// TestAPILogs_SinceFilter verifies the apilogs since filter contract.
// Asserts that status = , want 200.
func TestAPILogs_SinceFilter(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	old := time.Now().Add(-1 * time.Hour)
	recent := time.Now()
	h.logBuffer.Add(telemetry.LogEntry{Time: old, Level: "INFO", Message: "old"})
	h.logBuffer.Add(telemetry.LogEntry{Time: recent, Level: "INFO", Message: "new"})

	since := time.Now().Add(-10 * time.Minute).Format(time.RFC3339)
	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/logs?since="+since, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Result().StatusCode)
	}
	var lr logsResponse
	if err := json.NewDecoder(w.Result().Body).Decode(&lr); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if len(lr.Entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(lr.Entries))
	}
	if lr.Entries[0].Message != "new" {
		t.Errorf("message = %q, want 'new'", lr.Entries[0].Message)
	}
}

// TestAPILogs_LimitFilter verifies the apilogs limit filter contract.
// Asserts that status = , want 200.
func TestAPILogs_LimitFilter(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	for i := range 20 {
		h.logBuffer.Add(telemetry.LogEntry{Level: "INFO", Message: "msg", Attrs: map[string]any{"i": i}})
	}

	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/logs?limit=5", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Result().StatusCode)
	}
	var lr logsResponse
	if err := json.NewDecoder(w.Result().Body).Decode(&lr); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if len(lr.Entries) != 5 {
		t.Fatalf("got %d entries, want 5", len(lr.Entries))
	}
}

// TestAPILogs_ComponentFilter verifies the apilogs component filter contract.
// Asserts that status = , want 200.
func TestAPILogs_ComponentFilter(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	h.logBuffer.Add(telemetry.LogEntry{Level: "INFO", Message: "a", Attrs: map[string]any{"component": "server"}})
	h.logBuffer.Add(telemetry.LogEntry{Level: "INFO", Message: "b", Attrs: map[string]any{"component": "storage"}})

	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/logs?component=server", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Result().StatusCode)
	}
	var lr logsResponse
	if err := json.NewDecoder(w.Result().Body).Decode(&lr); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if len(lr.Entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(lr.Entries))
	}
	if lr.Entries[0].Message != "a" {
		t.Errorf("message = %q, want 'a'", lr.Entries[0].Message)
	}
}

// TestAPILogs_BeforeFilter verifies the apilogs before filter contract.
// Asserts that status = , want 200.
func TestAPILogs_BeforeFilter(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	old := time.Now().Add(-1 * time.Hour)
	recent := time.Now()
	h.logBuffer.Add(telemetry.LogEntry{Time: old, Level: "INFO", Message: "old"})
	h.logBuffer.Add(telemetry.LogEntry{Time: recent, Level: "INFO", Message: "new"})

	before := time.Now().Add(-10 * time.Minute).Format(time.RFC3339)
	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/logs?before="+before, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Result().StatusCode)
	}
	var lr logsResponse
	if err := json.NewDecoder(w.Result().Body).Decode(&lr); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if len(lr.Entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(lr.Entries))
	}
	if lr.Entries[0].Message != "old" {
		t.Errorf("message = %q, want 'old'", lr.Entries[0].Message)
	}
}

// TestAPILogs_HasMore verifies the apilogs has more contract.
// Asserts that status = , want 200.
func TestAPILogs_HasMore(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	for i := range 10 {
		h.logBuffer.Add(telemetry.LogEntry{
			Time:    time.Now().Add(time.Duration(i) * time.Second),
			Level:   "INFO",
			Message: "msg",
			Attrs:   map[string]any{"i": i},
		})
	}

	// Request limit=5 with 10 entries available  -  should get hasMore=true.
	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/logs?limit=5", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Result().StatusCode)
	}
	var lr logsResponse
	if err := json.NewDecoder(w.Result().Body).Decode(&lr); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if len(lr.Entries) != 5 {
		t.Fatalf("got %d entries, want 5", len(lr.Entries))
	}
	if !lr.HasMore {
		t.Error("hasMore = false, want true")
	}

	// Request limit=20 with 10 entries  -  should get hasMore=false.
	req2 := authedRequest(t, h, mux, http.MethodGet, "/ui/api/logs?limit=20", nil)
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)

	var lr2 logsResponse
	if err := json.NewDecoder(w2.Result().Body).Decode(&lr2); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if len(lr2.Entries) != 10 {
		t.Fatalf("got %d entries, want 10", len(lr2.Entries))
	}
	if lr2.HasMore {
		t.Error("hasMore = true, want false")
	}
}

// -------------------------------------------------------------------------
// BRUTE-FORCE PROTECTION
// -------------------------------------------------------------------------

// TestLogin_BruteForceProtection verifies the login brute force protection contract.
// Asserts that status = , want.
func TestLogin_BruteForceProtection(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)
	lt := httputil.NewLoginThrottle(3, 5*time.Minute)
	defer lt.Close()
	h.loginThrottle = lt

	// 3 bad attempts
	for range 3 {
		form := url.Values{"access_key": {"wrong"}, "secret_key": {"wrong"}}
		req := httptest.NewRequest(http.MethodPost, "/ui/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "10.0.0.1:12345"
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
	}

	// 4th attempt should be 429
	form := url.Values{"access_key": {"wrong"}, "secret_key": {"wrong"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "10.0.0.1:12345"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d", w.Code, http.StatusTooManyRequests)
	}
}

// BenchmarkLogin_TimingParity verifies that login attempts with an invalid
// access key take approximately the same time as attempts with a valid key
// but wrong secret. Both should be dominated by checkSecret (bcrypt when
// configured). A large disparity would indicate a timing side-channel.
// BenchmarkLogin_InvalidKey benchmarks login_invalid key.
// BenchmarkLogin_InvalidKey benchmarks login_invalid key.
func BenchmarkLogin_InvalidKey(b *testing.B) {
	h, mux := benchLoginHandler(b)
	_ = h

	form := url.Values{"access_key": {"wrong-key"}, "secret_key": {"wrong-secret"}}
	body := form.Encode()

	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest(http.MethodPost, "/ui/login", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
	}
}

// BenchmarkLogin_ValidKeyWrongSecret measures the login valid key wrong secret path by exercising form.Encode, httptest.NewRequest, strings.NewReader.
func BenchmarkLogin_ValidKeyWrongSecret(b *testing.B) {
	h, mux := benchLoginHandler(b)
	_ = h

	form := url.Values{"access_key": {testAdminKey}, "secret_key": {"wrong-secret"}}
	body := form.Encode()

	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest(http.MethodPost, "/ui/login", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
	}
}

// benchLoginHandler builds a handler with a bcrypt-hashed admin secret for
// login timing benchmarks.
func benchLoginHandler(b *testing.B) (*Handler, *http.ServeMux) {
	b.Helper()

	bcryptHash, err := bcrypt.GenerateFromPassword([]byte(testAdminSecret), bcrypt.DefaultCost)
	if err != nil {
		b.Fatal(err)
	}

	mockStore := testutil.NewMockStore(b)
	mockStore.GetQuotaStatsResp = map[string]core.QuotaStat{}
	mockStore.GetObjectCountsResp = map[string]int64{}
	mockStore.GetActiveMultipartResp = map[string]int64{}
	mockStore.GetUsageForPeriodResp = map[string]core.UsageStat{}
	mockStore.ListDirChildrenResp = &core.DirectoryListResult{}

	mgr := proxy.NewBackendManager(&proxy.BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{},
		Stores:          mockStore,
		Dashboard:       mockStore,
		Metrics:         mockStore,
		Order:           []string{},
		RoutingStrategy: config.RoutingPack,
	})
	proxytest.AttachWorkers(mgr, mockStore)
	b.Cleanup(mgr.Close)

	cfg := &config.Config{
		RoutingStrategy: config.RoutingPack,
		Replication:     config.ReplicationConfig{Factor: 1},
		RateLimit:       config.RateLimitConfig{Enabled: false},
		UI: config.UIConfig{
			Enabled:       true,
			AdminKey:      testAdminKey,
			AdminSecret:   string(bcryptHash),
			SessionSecret: testSessionSecret,
		},
	}

	h := New(&Deps{BackendOps: mgr, Objects: mgr.ObjectManager, Rebalancer: mgr.Rebalancer, OverRep: mgr.OverReplicationCleaner, DBHealthy: func() bool { return true }, Cfg: cfg, LogBuffer: telemetry.NewLogBuffer()})
	mux := http.NewServeMux()
	h.Register(mux, "/ui")

	return h, mux
}

// -------------------------------------------------------------------------
// DOWNLOAD API TESTS
// -------------------------------------------------------------------------

// TestDownload_MethodNotAllowed verifies the download method not allowed contract.
// Asserts that status = , want.
func TestDownload_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)
	req := authedRequest(t, h, mux, http.MethodPost, "/ui/api/download?key=test-bucket/file.txt", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// TestDownload_MissingKey verifies the download missing key contract.
// Asserts that status = , want.
func TestDownload_MissingKey(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)
	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/download", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestDownload_InvalidBucketPrefix verifies the download invalid bucket prefix contract.
// Asserts that status = , want.
func TestDownload_InvalidBucketPrefix(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)
	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/download?key=no-such-bucket/file.txt", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestDownload_NotFound verifies the download not found contract.
// Asserts that status = , want.
func TestDownload_NotFound(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)
	// Default mock store returns ErrObjectNotFound for GetAllObjectLocations
	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/download?key=test-bucket/missing.txt", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

// TestDownload_StoreError verifies the download store error contract.
// Asserts that status = , want.
func TestDownload_StoreError(t *testing.T) {
	t.Parallel()
	h, mux, mock := newTestHandlerWithMock(t)
	mock.GetAllLocationsErr = errors.New("db down")

	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/download?key=test-bucket/file.txt", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
}

// -------------------------------------------------------------------------
// CLEAN EXCESS API TESTS
// -------------------------------------------------------------------------

// TestCleanExcess_MethodNotAllowed verifies the clean excess method not allowed contract.
// Asserts that status = , want.
func TestCleanExcess_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)
	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/clean-excess", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// -------------------------------------------------------------------------
// UPLOAD API ERROR PATHS
// -------------------------------------------------------------------------

// TestUpload_InvalidMultipartForm verifies the upload invalid multipart form contract.
// Asserts that status = , want.
func TestUpload_InvalidMultipartForm(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)
	// Send a POST with a non-multipart body to trigger ParseMultipartForm failure
	req := authedRequest(t, h, mux, http.MethodPost, "/ui/api/upload", strings.NewReader("not-a-form"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// -------------------------------------------------------------------------
// BRUTE FORCE TESTS
// -------------------------------------------------------------------------

// TestLogin_BruteForceReset verifies the login brute force reset contract.
// Asserts that login status = , want.
func TestLogin_BruteForceReset(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)
	lt := httputil.NewLoginThrottle(3, 5*time.Minute)
	defer lt.Close()
	h.loginThrottle = lt

	addr := "10.0.0.1:12345"

	// 2 bad attempts
	for range 2 {
		form := url.Values{"access_key": {"wrong"}, "secret_key": {"wrong"}}
		req := httptest.NewRequest(http.MethodPost, "/ui/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = addr
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
	}

	// Successful login resets counter
	form := url.Values{"access_key": {testAdminKey}, "secret_key": {testAdminSecret}}
	req := httptest.NewRequest(http.MethodPost, "/ui/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = addr
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, want %d", w.Code, http.StatusSeeOther)
	}

	// 2 more bad attempts should not trigger lockout (counter was reset)
	for range 2 {
		form := url.Values{"access_key": {"wrong"}, "secret_key": {"wrong"}}
		req := httptest.NewRequest(http.MethodPost, "/ui/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = addr
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)

		if w.Code == http.StatusTooManyRequests {
			t.Error("should not be locked out after success reset and 2 new failures")
		}
	}
}

// -------------------------------------------------------------------------
// CLEAN-EXCESS (OVER-REPLICATION CLEANUP)
// -------------------------------------------------------------------------

// TestAPICleanExcess_FactorLeOne covers the short-circuit branch: with the
// default replication factor of 1, the handler returns 200 immediately with
// removed=0 rather than kicking off a background job.
func TestAPICleanExcess_FactorLeOne(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodPost, "/ui/api/clean-excess", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["ok"] != true {
		t.Errorf("ok = %v, want true", resp["ok"])
	}
	// Use JSON unmarshaling's json.Number behaviour: removed comes through as float64.
	if removed, _ := resp["removed"].(float64); removed != 0 {
		t.Errorf("removed = %v, want 0", resp["removed"])
	}
}

// TestAPICleanExcess_WrongMethod covers the GET-rejection path.
func TestAPICleanExcess_WrongMethod(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/clean-excess", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

// TestAPICleanExcess_RequiresAuth verifies unauthenticated POSTs are rejected.
func TestAPICleanExcess_RequiresAuth(t *testing.T) {
	t.Parallel()
	_, mux := newTestHandler(t)

	req := httptest.NewRequest(http.MethodPost, "/ui/api/clean-excess", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 401/403", w.Code)
	}
}

// TestAPICleanExcessStatus_IdleByDefault verifies the status endpoint
// reports "idle" when no cleanup has ever run.
func TestAPICleanExcessStatus_IdleByDefault(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/clean-excess/status", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "idle" {
		t.Errorf("status = %v, want %q", resp["status"], "idle")
	}
}

// TestAPICleanExcessStatus_RequiresAuth verifies the status endpoint rejects
// unauthenticated requests.
func TestAPICleanExcessStatus_RequiresAuth(t *testing.T) {
	t.Parallel()
	_, mux := newTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/ui/api/clean-excess/status", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 401/403", w.Code)
	}
}
