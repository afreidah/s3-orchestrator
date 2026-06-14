// -------------------------------------------------------------------------------
// Admin API Handler Tests
//
// Author: Alex Freidah
//
// Unit tests for the admin API endpoints including authentication, status,
// object locations, cleanup queue, usage flush, replication, and log level.
// -------------------------------------------------------------------------------

package admin

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
)

// TestRequireToken_Missing verifies the require token missing contract.
// Asserts that status = , want.
func TestRequireToken_Missing(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/admin/api/status", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

// TestRequireToken_Wrong verifies the require token wrong contract.
// Asserts that status = , want.
func TestRequireToken_Wrong(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/admin/api/status", nil)
	req.Header.Set("X-Admin-Token", "wrong-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

// TestRequireToken_Valid verifies the require token valid contract.
// Asserts that status = , want.
func TestRequireToken_Valid(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/admin/api/log-level", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

// TestLogLevel_Get verifies the log level get contract.
// Asserts that status = , want.
func TestLogLevel_Get(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/admin/api/log-level", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp["level"] != "info" {
		t.Errorf("level = %q, want %q", resp["level"], "info")
	}
}

// TestLogLevel_Put verifies the log level put contract.
// Asserts that status = , want.
func TestLogLevel_Put(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPut, "/admin/api/log-level",
		strings.NewReader(`{"level":"debug"}`))
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp["level"] != "debug" {
		t.Errorf("level = %q, want %q", resp["level"], "debug")
	}

	// Verify the level actually changed
	if h.logLevel.Level() != slog.LevelDebug {
		t.Errorf("logLevel = %v, want %v", h.logLevel.Level(), slog.LevelDebug)
	}
}

// TestLogLevel_PutInvalidJSON verifies the log level put invalid json contract.
// Asserts that status = , want.
func TestLogLevel_PutInvalidJSON(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPut, "/admin/api/log-level",
		strings.NewReader(`not json`))
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestLogLevel_MethodNotAllowed verifies the log level method not allowed contract.
// Asserts that status = , want.
func TestLogLevel_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/admin/api/log-level", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

// TestReloadStatus_NoReloadYet returns a placeholder when the
// reload provider has not been wired (no SIGHUP has happened yet).
func TestReloadStatus_NoReloadYet(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/admin/api/reload-status", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "no_reload_yet") {
		t.Errorf("body = %q, want no_reload_yet placeholder", w.Body.String())
	}
}

// TestReloadStatus_ReturnsProvidedResult exercises the SetReloadStatus
// Provider hook the runtime calls after building the reload coordinator.
func TestReloadStatus_ReturnsProvidedResult(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	h.SetReloadStatusProvider(func() any {
		return map[string]any{"generation": 7, "status": "full_success"}
	})
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/admin/api/reload-status", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"generation":7`) {
		t.Errorf("body = %q, want generation field", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "full_success") {
		t.Errorf("body = %q, want status field", w.Body.String())
	}
}

// TestStatus_MethodNotAllowed verifies the status method not allowed contract.
// Asserts that status = , want.
func TestStatus_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/admin/api/status", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

// TestObjectLocations_MissingKey verifies the object locations missing key contract.
// Asserts that status = , want.
func TestObjectLocations_MissingKey(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/admin/api/object-locations", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestUsageFlush_MethodNotAllowed verifies the usage flush method not allowed contract.
// Asserts that status = , want.
func TestUsageFlush_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/admin/api/usage-flush", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

// TestReplicate_MethodNotAllowed verifies the replicate method not allowed contract.
// Asserts that status = , want.
func TestReplicate_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/admin/api/replicate", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

// -------------------------------------------------------------------------
// CACHE-FLUSH TESTS
// -------------------------------------------------------------------------

// TestCacheFlush_Disabled verifies that POST /admin/api/cache/flush returns
// 503 when the orchestrator is configured without an object data cache,
// so callers can distinguish "no cache" from "cache empty after flush."
func TestCacheFlush_Disabled(t *testing.T) {
	t.Parallel()
	h := newTestHandler() // objectCache is nil
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/admin/api/cache/flush", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp["status"] != "disabled" {
		t.Errorf("status field = %q, want %q", resp["status"], "disabled")
	}
}

// TestCacheFlush_Empty verifies a flush against an empty cache returns
// 200 with entries_cleared=0, distinguishing the disabled case (503).
func TestCacheFlush_Empty(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithCache(t)
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/admin/api/cache/flush", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp["status"] != "flushed" {
		t.Errorf("status = %v, want flushed", resp["status"])
	}
	if got := resp["entries_cleared"]; got != float64(0) {
		t.Errorf("entries_cleared = %v, want 0", got)
	}
}

// TestCacheFlush_Cleared verifies the entry count returned by the flush
// matches the number of entries that were in the cache at flush time.
func TestCacheFlush_Cleared(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithCache(t)
	mc := h.objectCache.(*cache.MemoryCache)
	for _, k := range []string{"a", "b", "c"} {
		mc.PutBytes(k, []byte(k+"-data"), cache.EntryMeta{})
	}

	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/admin/api/cache/flush", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if got := resp["entries_cleared"]; got != float64(3) {
		t.Errorf("entries_cleared = %v, want 3", got)
	}
	if mc.Stats().Entries != 0 {
		t.Errorf("cache still has entries after flush: %d", mc.Stats().Entries)
	}
}

// TestCacheStats_Disabled verifies stats endpoint returns 503 when the
// cache is not configured, distinguishing it from "stats valid but zero."
func TestCacheStats_Disabled(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/admin/api/cache", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

// TestCacheStats_Populated verifies the stats endpoint reports the
// running cache's actual entry count and size.
func TestCacheStats_Populated(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithCache(t)
	mc := h.objectCache.(*cache.MemoryCache)
	mc.PutBytes("key-a", []byte("hello"), cache.EntryMeta{ContentType: "text/plain"})
	mc.PutBytes("key-b", []byte("world"), cache.EntryMeta{ContentType: "text/plain"})

	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/admin/api/cache", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if got := resp["entries"]; got != float64(2) {
		t.Errorf("entries = %v, want 2", got)
	}
	if got := resp["size_bytes"]; got.(float64) <= 0 {
		t.Errorf("size_bytes = %v, want > 0", got)
	}
	if got := resp["max_bytes"]; got != float64(1024*1024) {
		t.Errorf("max_bytes = %v, want %d", got, 1024*1024)
	}
}

// TestCacheInvalidateKey_RemovesEntry verifies a key invalidation makes
// the targeted entry miss while leaving siblings intact.
func TestCacheInvalidateKey_RemovesEntry(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithCache(t)
	mc := h.objectCache.(*cache.MemoryCache)
	mc.PutBytes("a/1", []byte("aaa"), cache.EntryMeta{})
	mc.PutBytes("a/2", []byte("bbb"), cache.EntryMeta{})

	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/admin/api/cache/keys/a/1", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp["key"] != "a/1" {
		t.Errorf("key = %v, want a/1", resp["key"])
	}

	if _, ok := mc.Get("a/1"); ok {
		t.Error("expected miss for invalidated key")
	}
	if _, ok := mc.Get("a/2"); !ok {
		t.Error("sibling key dropped unexpectedly")
	}
}

// TestCacheInvalidateKey_UnknownKey verifies invalidation of a non-
// existent key returns 200 (Invalidate is a no-op for unknown keys).
func TestCacheInvalidateKey_UnknownKey(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithCache(t)
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/admin/api/cache/keys/nonexistent", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

// TestCacheInvalidatePrefix_DropsMatching verifies the prefix endpoint
// drops every matching key, leaves outsiders intact, and reports the
// drop count.
func TestCacheInvalidatePrefix_DropsMatching(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithCache(t)
	mc := h.objectCache.(*cache.MemoryCache)
	for _, k := range []string{"users/1/a", "users/1/b", "users/2/c", "logs/1/x"} {
		mc.PutBytes(k, []byte("data"), cache.EntryMeta{})
	}

	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/admin/api/cache/prefix?prefix=users/1/", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if got := resp["entries_dropped"]; got != float64(2) {
		t.Errorf("entries_dropped = %v, want 2", got)
	}
	if _, ok := mc.Get("users/2/c"); !ok {
		t.Error("expected hit for users/2/c (outside invalidated prefix)")
	}
}

// TestCacheInvalidatePrefix_EmptyRejected verifies an empty prefix is
// rejected with 400 so operators don't accidentally drop the cache via
// a missing query param; full flush is its own dedicated endpoint.
func TestCacheInvalidatePrefix_EmptyRejected(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithCache(t)
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/admin/api/cache/prefix", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// TestCacheInvalidateKey_Disabled verifies the per-key invalidate
// endpoint reports 503 when the cache is not configured, so callers
// can distinguish a no-op invalidation from a missing cache.
func TestCacheInvalidateKey_Disabled(t *testing.T) {
	t.Parallel()
	h := newTestHandler() // objectCache is nil
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/admin/api/cache/keys/foo", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// TestCacheInvalidatePrefix_Disabled verifies the prefix invalidate
// endpoint reports 503 when the cache is not configured.
func TestCacheInvalidatePrefix_Disabled(t *testing.T) {
	t.Parallel()
	h := newTestHandler() // objectCache is nil
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/admin/api/cache/prefix?prefix=foo/", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// newTestHandler constructs a new test handler.
func newTestHandler() *Handler {
	var lv slog.LevelVar
	lv.Set(slog.LevelInfo)
	return &Handler{
		log:        slog.Default().With(logfmt.Component("admin")),
		runtimeOps: fakeRuntimeOps{},
		token:      "test-token",
		logLevel:   &lv,
	}
}

// newTestHandlerWithCache constructs a test handler with a real
// MemoryCache attached so cache-flush tests exercise the full path
// without a hand-rolled fake.
func newTestHandlerWithCache(t *testing.T) *Handler {
	t.Helper()
	mc, err := cache.NewMemoryCache(cache.MemoryConfig{
		MaxSize:       1024 * 1024,
		MaxObjectSize: 1024,
		TTL:           time.Minute,
	})
	if err != nil {
		t.Fatalf("NewMemoryCache: %v", err)
	}
	h := newTestHandler()
	h.objectCache = mc
	return h
}

// -------------------------------------------------------------------------
// DECRYPT-EXISTING TESTS
// -------------------------------------------------------------------------

// TestDecryptExisting_NoEncryptor verifies the decrypt existing no encryptor contract.
// Asserts that status = , want.
func TestDecryptExisting_NoEncryptor(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/admin/api/decrypt-existing", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp["error"] != "encryption not enabled" {
		t.Errorf("error = %q, want %q", resp["error"], "encryption not enabled")
	}
}

// TestDecryptExisting_MethodNotAllowed verifies the decrypt existing method not allowed contract.
// Asserts that status = , want.
func TestDecryptExisting_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/admin/api/decrypt-existing", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

// TestEncryptExisting_NoEncryptor verifies the encrypt existing no encryptor contract.
// Asserts that status = , want.
func TestEncryptExisting_NoEncryptor(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/admin/api/encrypt-existing", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// -------------------------------------------------------------------------
// REMOVE-BACKEND CONFIRMATION TESTS
// -------------------------------------------------------------------------

// TestRemoveToken_RoundTrip verifies the remove token round trip behaviour described by the test name.
func TestRemoveToken_RoundTrip(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	token := h.generateRemoveToken("my-backend")

	if !h.validRemoveToken(token, "my-backend") {
		t.Error("valid token should pass validation")
	}
}

// TestRemoveToken_WrongBackend verifies the remove token wrong backend behaviour described by the test name.
func TestRemoveToken_WrongBackend(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	token := h.generateRemoveToken("backend-a")

	if h.validRemoveToken(token, "backend-b") {
		t.Error("token for backend-a should not validate for backend-b")
	}
}

// TestRemoveToken_Tampered verifies the remove token tampered behaviour described by the test name.
func TestRemoveToken_Tampered(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	token := h.generateRemoveToken("my-backend")

	if h.validRemoveToken(token+"x", "my-backend") {
		t.Error("tampered token should fail validation")
	}
}

// TestRemoveToken_Empty verifies the remove token empty behaviour described by the test name.
func TestRemoveToken_Empty(t *testing.T) {
	t.Parallel()
	h := newTestHandler()

	if h.validRemoveToken("", "my-backend") {
		t.Error("empty token should fail validation")
	}
}

// TestRemoveToken_MalformedBase64 verifies the remove token malformed base64 behaviour described by the test name.
func TestRemoveToken_MalformedBase64(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	if h.validRemoveToken("not-valid-base64!!!.also-bad!!!", "my-backend") {
		t.Error("malformed base64 should fail")
	}
}

// TestRemoveToken_NoDot verifies the remove token no dot behaviour described by the test name.
func TestRemoveToken_NoDot(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	if h.validRemoveToken("nodotinthisstring", "my-backend") {
		t.Error("token without dot separator should fail")
	}
}

// TestRemoveToken_BadPayloadFormat verifies the remove token bad payload format path by exercising hmac.New, mac.Write, mac.Sum.
func TestRemoveToken_BadPayloadFormat(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	// Valid base64 but wrong payload structure (not "purge|name|expiry")
	payload := base64.RawURLEncoding.EncodeToString([]byte("wrong|format"))
	mac := hmac.New(sha256.New, []byte("test-token"))
	mac.Write([]byte("wrong|format"))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	token := payload + "." + sig
	if h.validRemoveToken(token, "my-backend") {
		t.Error("wrong payload format should fail")
	}
}

// TestRemoveToken_WrongKey verifies the remove token wrong key behaviour described by the test name.
func TestRemoveToken_WrongKey(t *testing.T) {
	t.Parallel()
	h1 := newTestHandler()
	h2 := &Handler{token: "different-key"}

	token := h1.generateRemoveToken("my-backend")
	if h2.validRemoveToken(token, "my-backend") {
		t.Error("token signed with different key should fail")
	}
}
