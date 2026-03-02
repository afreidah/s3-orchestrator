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
