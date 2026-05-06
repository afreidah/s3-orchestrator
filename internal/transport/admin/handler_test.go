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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
)

// TestRequireToken_Missing verifies the require token missing contract.
// Asserts that status = , want.
func TestRequireToken_Missing(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/admin/api/status", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/admin/api/status", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/admin/api/log-level", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/admin/api/log-level", nil)
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

	req := httptest.NewRequest(http.MethodPut, "/admin/api/log-level",
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

	req := httptest.NewRequest(http.MethodPut, "/admin/api/log-level",
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

	req := httptest.NewRequest(http.MethodDelete, "/admin/api/log-level", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

// TestStatus_MethodNotAllowed verifies the status method not allowed contract.
// Asserts that status = , want.
func TestStatus_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest(http.MethodPost, "/admin/api/status", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/admin/api/object-locations", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/admin/api/usage-flush", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/admin/api/replicate", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
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
		log:      slog.Default().With(logfmt.Component("admin")),
		token:    "test-token",
		logLevel: &lv,
	}
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

	req := httptest.NewRequest(http.MethodPost, "/admin/api/decrypt-existing", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/admin/api/decrypt-existing", nil)
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

	req := httptest.NewRequest(http.MethodPost, "/admin/api/encrypt-existing", nil)
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
