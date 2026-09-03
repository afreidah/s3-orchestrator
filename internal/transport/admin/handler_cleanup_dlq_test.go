// -------------------------------------------------------------------------------
// Admin API - Cleanup DLQ Handler Tests
//
// Author: Alex Freidah
//
// Covers the cleanup-dlq list and requeue endpoints (route + typed response
// shape) plus the pure helpers parseLimit and cleanupDLQItems.
// -------------------------------------------------------------------------------

package admin

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// dlqErrStore is a core.CleanupStore whose DLQ methods return seeded errors so
// the handler's 500 branches can be exercised without a database. Only the
// methods the DLQ handlers call are overridden; the embedded nil satisfies the
// rest of the interface (never invoked on these paths).
type dlqErrStore struct {
	core.CleanupStore
	depthErr, listErr, requeueErr error
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

func (s dlqErrStore) CleanupDLQDepth(context.Context) (int64, error) { return 0, s.depthErr }
func (s dlqErrStore) ListCleanupDLQ(context.Context, string, int) ([]core.CleanupDLQItem, error) {
	return nil, s.listErr
}
func (s dlqErrStore) RequeueCleanupDLQ(context.Context, string) (int64, error) {
	return 0, s.requeueErr
}

// handlerWithCleanup builds a minimal handler wired to the given cleanup store.
func handlerWithCleanup(c core.CleanupStore) *Handler {
	return &Handler{log: slog.Default(), cleanup: c}
}

// TestHandleCleanupDLQ_ListReturnsTypedShape asserts the list endpoint answers
// 200 with the typed depth+items response shape.
func TestHandleCleanupDLQ_ListReturnsTypedShape(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodGet, "/admin/api/cleanup-dlq?backend=b2&limit=25", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Depth int64 `json:"depth"`
		Items []struct {
			Backend string `json:"backend"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
}

// TestHandleCleanupDLQRequeue_ReturnsCount asserts the requeue endpoint answers
// 200 and echoes the scoped backend in the typed response.
func TestHandleCleanupDLQRequeue_ReturnsCount(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodPost, "/admin/api/cleanup-dlq/requeue?backend=b2", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Backend  string `json:"backend"`
		Requeued int64  `json:"requeued"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	if resp.Backend != "b2" {
		t.Errorf("backend = %q, want b2", resp.Backend)
	}
}

// TestHandleCleanupDLQ_ErrorPaths asserts the list endpoint returns 500 when
// either the depth read or the listing fails.
func TestHandleCleanupDLQ_ErrorPaths(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	cases := []struct {
		name  string
		store core.CleanupStore
	}{
		{"depth error", dlqErrStore{depthErr: boom}},
		{"list error", dlqErrStore{listErr: boom}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := handlerWithCleanup(c.store)
			w := httptest.NewRecorder()
			h.handleCleanupDLQ(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/api/cleanup-dlq", nil))
			if w.Code != http.StatusInternalServerError {
				t.Errorf("status = %d, want 500", w.Code)
			}
		})
	}
}

// TestHandleCleanupDLQRequeue_ErrorPath asserts the requeue endpoint returns
// 500 when the store requeue fails.
func TestHandleCleanupDLQRequeue_ErrorPath(t *testing.T) {
	t.Parallel()
	h := handlerWithCleanup(dlqErrStore{requeueErr: errors.New("boom")})
	w := httptest.NewRecorder()
	h.handleCleanupDLQRequeue(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/admin/api/cleanup-dlq/requeue", nil))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// TestParseLimit covers the default, clamp, and parse-failure branches of the
// limit query parser.
func TestParseLimit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw                 string
		def, maxLimit, want int
	}{
		{"", 50, 500, 50},      // unset -> default
		{"abc", 50, 500, 50},   // unparseable -> default
		{"0", 50, 500, 50},     // non-positive -> default
		{"-5", 50, 500, 50},    // negative -> default
		{"25", 50, 500, 25},    // in range
		{"9000", 50, 500, 500}, // over cap -> clamped
	}
	for _, c := range cases {
		if got := parseLimit(c.raw, c.def, c.maxLimit); got != c.want {
			t.Errorf("parseLimit(%q) = %d, want %d", c.raw, got, c.want)
		}
	}
}

// TestCleanupDLQItems_Mapping asserts the store-to-wire item mapping carries
// every field through unchanged.
func TestCleanupDLQItems_Mapping(t *testing.T) {
	t.Parallel()
	moved := time.Now()
	in := []core.CleanupDLQItem{{
		BackendName: "b2", ObjectKey: "k1", Reason: "delete_failed",
		SizeBytes: 123, Attempts: 10, MovedAt: moved, LastError: "backend unavailable",
	}}
	out := cleanupDLQItems(in)
	if len(out) != 1 {
		t.Fatalf("mapped %d, want 1", len(out))
	}
	if out[0].Backend != "b2" || out[0].ObjectKey != "k1" || out[0].SizeBytes != 123 ||
		out[0].Attempts != 10 || out[0].LastError != "backend unavailable" {
		t.Errorf("mapping wrong: %+v", out[0])
	}
}
