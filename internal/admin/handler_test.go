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
)

func TestRequireToken_Missing(t *testing.T) {
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

func TestRequireToken_Wrong(t *testing.T) {
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

func TestRequireToken_Valid(t *testing.T) {
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

func TestLogLevel_Get(t *testing.T) {
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

func TestLogLevel_Put(t *testing.T) {
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

func TestLogLevel_PutInvalidJSON(t *testing.T) {
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

func TestLogLevel_MethodNotAllowed(t *testing.T) {
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

func TestStatus_MethodNotAllowed(t *testing.T) {
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

func TestObjectLocations_MissingKey(t *testing.T) {
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

func TestUsageFlush_MethodNotAllowed(t *testing.T) {
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

func TestReplicate_MethodNotAllowed(t *testing.T) {
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

func newTestHandler() *Handler {
	var lv slog.LevelVar
	lv.Set(slog.LevelInfo)
	return &Handler{
		token:    "test-token",
		logLevel: &lv,
	}
}

// -------------------------------------------------------------------------
// DECRYPT-EXISTING TESTS
// -------------------------------------------------------------------------

func TestDecryptExisting_NoEncryptor(t *testing.T) {
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

func TestDecryptExisting_MethodNotAllowed(t *testing.T) {
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

func TestEncryptExisting_NoEncryptor(t *testing.T) {
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

func TestRemoveToken_RoundTrip(t *testing.T) {
	h := newTestHandler()
	token := h.generateRemoveToken("my-backend")

	if !h.validRemoveToken(token, "my-backend") {
		t.Error("valid token should pass validation")
	}
}

func TestRemoveToken_WrongBackend(t *testing.T) {
	h := newTestHandler()
	token := h.generateRemoveToken("backend-a")

	if h.validRemoveToken(token, "backend-b") {
		t.Error("token for backend-a should not validate for backend-b")
	}
}

func TestRemoveToken_Tampered(t *testing.T) {
	h := newTestHandler()
	token := h.generateRemoveToken("my-backend")

	if h.validRemoveToken(token+"x", "my-backend") {
		t.Error("tampered token should fail validation")
	}
}

func TestRemoveToken_Empty(t *testing.T) {
	h := newTestHandler()

	if h.validRemoveToken("", "my-backend") {
		t.Error("empty token should fail validation")
	}
}

func TestRemoveToken_MalformedBase64(t *testing.T) {
	h := newTestHandler()
	if h.validRemoveToken("not-valid-base64!!!.also-bad!!!", "my-backend") {
		t.Error("malformed base64 should fail")
	}
}

func TestRemoveToken_NoDot(t *testing.T) {
	h := newTestHandler()
	if h.validRemoveToken("nodotinthisstring", "my-backend") {
		t.Error("token without dot separator should fail")
	}
}

func TestRemoveToken_BadPayloadFormat(t *testing.T) {
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

func TestRemoveToken_WrongKey(t *testing.T) {
	h1 := newTestHandler()
	h2 := &Handler{token: "different-key"}

	token := h1.generateRemoveToken("my-backend")
	if h2.validRemoveToken(token, "my-backend") {
		t.Error("token signed with different key should fail")
	}
}
