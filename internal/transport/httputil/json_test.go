// -------------------------------------------------------------------------------
// HTTP JSON Helpers - Tests
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package httputil_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// TestWriteJSON checks the status code, content-type, and body encoding.
func TestWriteJSON(t *testing.T) {
	t.Parallel()
	w := httptest.NewRecorder()
	httputil.WriteJSON(w, http.StatusCreated, map[string]int{"n": 42})

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", w.Code, http.StatusCreated)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var decoded map[string]int
	if err := json.NewDecoder(w.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if decoded["n"] != 42 {
		t.Errorf("decoded n = %d, want 42", decoded["n"])
	}
}

// TestWriteJSONError encodes the error message safely; quote and
// backslash characters in the message must not corrupt the response.
func TestWriteJSONError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		msg  string
	}{
		{"plain", "something broke"},
		{"with quote", `bad "input"`},
		{"with backslash", `path\to\thing`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := httptest.NewRecorder()
			httputil.WriteJSONError(w, http.StatusBadRequest, tc.msg)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", w.Code)
			}
			var decoded map[string]string
			if err := json.NewDecoder(w.Body).Decode(&decoded); err != nil {
				t.Fatalf("decode body: %v (body=%q)", err, w.Body.String())
			}
			if decoded["error"] != tc.msg {
				t.Errorf("error = %q, want %q", decoded["error"], tc.msg)
			}
		})
	}
}

// TestDecodeJSONBody covers happy path, invalid JSON, and over-cap bodies.
func TestDecodeJSONBody(t *testing.T) {
	t.Parallel()

	t.Run("happy path", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"alice"}`))
		w := httptest.NewRecorder()
		var dst struct {
			Name string `json:"name"`
		}
		if !httputil.DecodeJSONBody(w, req, &dst, 1<<10) {
			t.Fatalf("DecodeJSONBody returned false: %s", w.Body.String())
		}
		if dst.Name != "alice" {
			t.Errorf("Name = %q, want alice", dst.Name)
		}
	})

	t.Run("invalid JSON returns 400", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{not json`))
		w := httptest.NewRecorder()
		var dst map[string]any
		if httputil.DecodeJSONBody(w, req, &dst, 1<<10) {
			t.Fatalf("DecodeJSONBody returned true on invalid JSON")
		}
		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})

	t.Run("over cap returns 400", func(t *testing.T) {
		t.Parallel()
		big := strings.Repeat(`a`, 2048)
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"v":"`+big+`"}`))
		w := httptest.NewRecorder()
		var dst map[string]string
		if httputil.DecodeJSONBody(w, req, &dst, 64) {
			t.Fatalf("DecodeJSONBody returned true on over-cap body")
		}
		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", w.Code)
		}
		// Drain remaining body so test recorder doesn't hold the limited reader.
		_, _ = io.Copy(io.Discard, req.Body)
	})
}

// TestRequireMethod accepts allowed methods and rejects others with
// Allow header + 405 JSON.
func TestRequireMethod(t *testing.T) {
	t.Parallel()

	t.Run("allowed", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		w := httptest.NewRecorder()
		if !httputil.RequireMethod(w, req, http.MethodPost, http.MethodPut) {
			t.Errorf("expected RequireMethod to allow POST")
		}
		if w.Code != http.StatusOK { // recorder default; nothing written
			t.Errorf("status touched on allowed path: %d", w.Code)
		}
	})

	t.Run("rejected", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		w := httptest.NewRecorder()
		if httputil.RequireMethod(w, req, http.MethodPost, http.MethodPut) {
			t.Errorf("expected RequireMethod to reject GET")
		}
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", w.Code)
		}
		if allow := w.Header().Get("Allow"); allow != "POST, PUT" {
			t.Errorf("Allow = %q, want %q", allow, "POST, PUT")
		}
		var decoded map[string]string
		if err := json.NewDecoder(w.Body).Decode(&decoded); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if decoded["error"] != httputil.ErrMethodNotAllowed {
			t.Errorf("error = %q, want %q", decoded["error"], httputil.ErrMethodNotAllowed)
		}
	})
}
