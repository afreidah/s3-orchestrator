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

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/storage"
	"github.com/afreidah/s3-orchestrator/internal/testutil"
)

const (
	testAdminKey    = "test-admin"
	testAdminSecret = "test-secret-key"
)

// newTestHandler builds a Handler wired to mock data for testing.
func newTestHandler(t *testing.T) (*Handler, *http.ServeMux) {
	t.Helper()
	h, mux, _ := newTestHandlerWithMock(t)
	return h, mux
}

// newTestHandlerWithMock builds a Handler and also returns the underlying mock
// store so tests can configure per-test error/response behaviour.
func newTestHandlerWithMock(t *testing.T) (*Handler, *http.ServeMux, *testutil.MockStore) {
	t.Helper()

	mockStore := &testutil.MockStore{
		GetQuotaStatsResp: map[string]storage.QuotaStat{
			"b1": {BackendName: "b1", BytesUsed: 500, BytesLimit: 1000},
		},
		GetObjectCountsResp:    map[string]int64{"b1": 42},
		GetActiveMultipartResp: map[string]int64{"b1": 0},
		GetUsageForPeriodResp:  map[string]storage.UsageStat{"b1": {APIRequests: 100}},
		ListDirChildrenResp: &storage.DirectoryListResult{
			Entries: []storage.DirEntry{
				{Name: "bucket1/", IsDir: true, FileCount: 10, TotalSize: 4096},
			},
		},
	}

	mgr := storage.NewBackendManager(&storage.BackendManagerConfig{
		Backends:        map[string]storage.ObjectBackend{},
		Store:           mockStore,
		Order:           []string{"b1"},
		RoutingStrategy: "pack",
	})
	t.Cleanup(mgr.Close)

	cfg := &config.Config{
		Buckets: []config.BucketConfig{
			{Name: "test-bucket"},
		},
		Backends: []config.BackendConfig{
			{Name: "b1", Endpoint: "https://s3.example.com", Bucket: "store",
				AccessKeyID: "AK", SecretAccessKey: "SK"},
		},
		RoutingStrategy: "pack",
		Replication:     config.ReplicationConfig{Factor: 1},
		RateLimit:       config.RateLimitConfig{Enabled: false},
		UI: config.UIConfig{
			Enabled:     true,
			AdminKey:    testAdminKey,
			AdminSecret: testAdminSecret,
		},
	}

	h := New(mgr, func() bool { return true }, cfg)

	mux := http.NewServeMux()
	h.Register(mux, "/ui")

	return h, mux, mockStore
}

// getSessionCookie performs a login and returns the session cookie.
func getSessionCookie(t *testing.T, h *Handler, mux *http.ServeMux) *http.Cookie {
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
		if c.Name == sessionCookieName {
			return c
		}
	}
	t.Fatal("login did not set session cookie")
	return nil
}

// authedRequest creates a request with a valid session cookie attached.
func authedRequest(t *testing.T, h *Handler, mux *http.ServeMux, method, path string, body io.Reader) *http.Request {
	t.Helper()
	cookie := getSessionCookie(t, h, mux)
	req := httptest.NewRequest(method, path, body)
	req.AddCookie(cookie)
	return req
}

// -------------------------------------------------------------------------
// AUTH TESTS
// -------------------------------------------------------------------------

func TestDashboard_RequiresAuth(t *testing.T) {
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

func TestAPIDashboard_RequiresAuth(t *testing.T) {
	_, mux := newTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/ui/api/dashboard", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Result().StatusCode)
	}
}

func TestLogin_ValidCredentials(t *testing.T) {
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

func TestLogin_InvalidCredentials(t *testing.T) {
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

func TestLogin_GET_ShowsForm(t *testing.T) {
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

func TestLogin_GET_RedirectsWhenAuthenticated(t *testing.T) {
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

func TestLogout_ClearsCookie(t *testing.T) {
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

func TestStaticAssets_NoAuthRequired(t *testing.T) {
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

func TestDashboard_Returns200HTML(t *testing.T) {
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

func TestAPIDashboard_ReturnsJSON(t *testing.T) {
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

	var data storage.DashboardData
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}
	if len(data.QuotaStats) == 0 {
		t.Error("expected non-empty QuotaStats")
	}
}

func TestTreeAPI_ReturnsJSON(t *testing.T) {
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

	var result storage.DirectoryListResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}
	if len(result.Entries) == 0 {
		t.Error("expected entries in tree response")
	}
}

func TestSecurityHeaders_PresentOnAllEndpoints(t *testing.T) {
	h, mux := newTestHandler(t)

	// Login page (no auth needed) and authenticated endpoints
	endpoints := []struct {
		path    string
		authed  bool
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
				"X-Frame-Options":        "DENY",
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

func TestUpdateConfig_ReflectsInDashboard(t *testing.T) {
	h, mux := newTestHandler(t)

	// Update config with a different routing strategy
	newCfg := &config.Config{
		Buckets: []config.BucketConfig{
			{Name: "updated-bucket"},
		},
		RoutingStrategy: "spread",
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

func TestAPIDelete_RequiresAuth(t *testing.T) {
	_, mux := newTestHandler(t)

	req := httptest.NewRequest(http.MethodPost, "/ui/api/delete", strings.NewReader(`{"key":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Result().StatusCode)
	}
}

func TestAPIUpload_RequiresAuth(t *testing.T) {
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

func TestAPIDelete_WrongMethod(t *testing.T) {
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/delete", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Result().StatusCode)
	}
}

func TestAPIDelete_BadJSON(t *testing.T) {
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodPost, "/ui/api/delete", strings.NewReader("{bad"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Result().StatusCode)
	}
}

func TestAPIDelete_EmptyKey(t *testing.T) {
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodPost, "/ui/api/delete", strings.NewReader(`{"key":""}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Result().StatusCode)
	}
}

func TestAPIDelete_Success(t *testing.T) {
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

func TestAPIDelete_ManagerError(t *testing.T) {
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

func TestAPIUpload_WrongMethod(t *testing.T) {
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/upload", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Result().StatusCode)
	}
}

func TestAPIUpload_MissingKey(t *testing.T) {
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

func TestAPIUpload_InvalidBucket(t *testing.T) {
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

func TestAPIUpload_MissingFile(t *testing.T) {
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

func TestAPIUpload_PutObjectError(t *testing.T) {
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

func TestAPIRebalance_RequiresAuth(t *testing.T) {
	_, mux := newTestHandler(t)

	req := httptest.NewRequest(http.MethodPost, "/ui/api/rebalance", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Result().StatusCode)
	}
}

func TestAPIRebalance_WrongMethod(t *testing.T) {
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/rebalance", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Result().StatusCode)
	}
}

func TestAPIRebalance_Success(t *testing.T) {
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodPost, "/ui/api/rebalance", nil)
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

func TestAPIRebalance_ManagerError(t *testing.T) {
	h, mux, mock := newTestHandlerWithMock(t)
	mock.GetQuotaStatsErr = errors.New("db down")

	req := authedRequest(t, h, mux, http.MethodPost, "/ui/api/rebalance", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Result().StatusCode)
	}
}

// -------------------------------------------------------------------------
// SYNC API TESTS
// -------------------------------------------------------------------------

func TestAPISync_RequiresAuth(t *testing.T) {
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

func TestAPISync_WrongMethod(t *testing.T) {
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/sync", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Result().StatusCode)
	}
}

func TestAPISync_BadJSON(t *testing.T) {
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodPost, "/ui/api/sync", strings.NewReader("{bad"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Result().StatusCode)
	}
}

func TestAPISync_EmptyFields(t *testing.T) {
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

func TestAPISync_UnknownBackend(t *testing.T) {
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
	if !strings.Contains(string(body), "unknown backend") {
		t.Errorf("expected unknown backend error, got: %s", body)
	}
}

func TestAPISync_UnknownBucket(t *testing.T) {
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
	if !strings.Contains(string(body), "unknown bucket") {
		t.Errorf("expected unknown bucket error, got: %s", body)
	}
}

func TestAPISync_ManagerError(t *testing.T) {
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

func TestLogin_UnsupportedMethod(t *testing.T) {
	_, mux := newTestHandler(t)

	req := httptest.NewRequest(http.MethodPut, "/ui/login", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Result().StatusCode)
	}
}

func TestDashboard_DataError(t *testing.T) {
	h, mux, mock := newTestHandlerWithMock(t)
	mock.GetQuotaStatsErr = errors.New("db down")

	req := authedRequest(t, h, mux, http.MethodGet, "/ui/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Result().StatusCode)
	}
}

func TestAPIDashboard_DataError(t *testing.T) {
	h, mux, mock := newTestHandlerWithMock(t)
	mock.GetQuotaStatsErr = errors.New("db down")

	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/dashboard", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Result().StatusCode)
	}
}

func TestTreeAPI_WithMaxKeys(t *testing.T) {
	h, mux := newTestHandler(t)

	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/tree?prefix=&maxKeys=50", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Result().StatusCode)
	}
}

func TestTreeAPI_DataError(t *testing.T) {
	h, mux, mock := newTestHandlerWithMock(t)
	mock.ListDirChildrenErr = errors.New("db down")

	req := authedRequest(t, h, mux, http.MethodGet, "/ui/api/tree?prefix=", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Result().StatusCode)
	}
}
