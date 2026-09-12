// -------------------------------------------------------------------------------
// Admin API - Backend Scope Tests
//
// Author: Alex Freidah
//
// Covers the backend parameter the maintenance passes accept: a name this
// instance serves reaches the pass, an unknown one is refused before any work
// starts, and an absent one leaves the pass fleet-wide.
//
// The refusal is the case worth pinning. A filter matching nothing looks
// exactly like a fleet with no work left, so a typo would otherwise report a
// clean pass over a backend that was never read.
// -------------------------------------------------------------------------------

package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestBackendParam_UnknownNameIsRefused asserts every pass refuses a backend
// this instance does not serve.
func TestBackendParam_UnknownNameIsRefused(t *testing.T) {
	t.Parallel()

	for _, path := range []string{
		"/admin/api/encrypt-existing",
		"/admin/api/decrypt-existing",
		"/admin/api/compress-existing",
		"/admin/api/decompress-existing",
		"/admin/api/backfill-checksums",
		"/admin/api/scrub",
	} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			h := newTestHandler(t)
			h.backendNames = func() []string { return []string{"b1", "b2"} }
			mux := http.NewServeMux()
			h.Register(mux)

			w := httptest.NewRecorder()
			mux.ServeHTTP(w, doAuth(http.MethodPost, path+"?backend=nosuch", ""))

			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body=%s", w.Code, w.Body.String())
			}
		})
	}
}

// TestBackendParam_KnownNameIsAccepted asserts a served backend passes the
// check, so the refusal above is the name being unknown rather than the
// parameter being rejected outright.
func TestBackendParam_KnownNameIsAccepted(t *testing.T) {
	t.Parallel()

	h := newTestHandler(t)
	h.backendNames = func() []string { return []string{"b1"} }

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/admin/api/scrub?backend=b1", nil)
	w := httptest.NewRecorder()
	got, ok := h.backendParam(w, req)

	if !ok {
		t.Fatalf("backendParam refused a served backend; body=%s", w.Body.String())
	}
	if got != "b1" {
		t.Errorf("backend = %q, want b1", got)
	}
}

// TestBackendParam_AbsentMeansEveryBackend pins that omitting the parameter is
// not the same as naming nothing: it is what a fleet-wide pass asks for.
func TestBackendParam_AbsentMeansEveryBackend(t *testing.T) {
	t.Parallel()

	h := newTestHandler(t)
	h.backendNames = func() []string { return []string{"b1"} }

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/admin/api/scrub", nil)
	w := httptest.NewRecorder()
	got, ok := h.backendParam(w, req)

	if !ok || got != "" {
		t.Errorf("backendParam = (%q, %v), want (\"\", true)", got, ok)
	}
}
