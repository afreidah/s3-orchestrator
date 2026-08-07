// -------------------------------------------------------------------------------
// Admin /workers Handler Tests
//
// Author: Alex Freidah
//
// Covers the operator-facing surface of the worker health snapshot:
//
//   - happy path: handler renders the provided snapshot as JSON
//   - lifecycle manager not wired -> 503 with a clear message
//   - auth path: missing token rejected like every other admin route
//
// The per-tick state that feeds this endpoint is exercised in
// internal/di/worker_health_test.go; here the contract is purely the
// HTTP response shape.
// -------------------------------------------------------------------------------

package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// TestHandleWorkers_HappyPath drives the success branch. The handler
// reads the supplied snapshot through the workerHealth callback and
// renders it under the "workers" key, mirroring the lifecycle manager
// registration order.
func TestHandleWorkers_HappyPath(t *testing.T) {
	t.Parallel()
	now := time.Unix(1715000000, 0)
	h := newTestHandler(t)
	h.workerHealth = func() []adminapi.WorkerHealth {
		return []adminapi.WorkerHealth{
			{Name: "cleanup_queue", LastSuccess: now, ConsecutiveFailures: 0},
			{Name: "replicator", LastFailure: now, LastError: "boom", ConsecutiveFailures: 3},
		}
	}
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/admin/api/workers", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Workers []adminapi.WorkerHealth `json:"workers"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Workers) != 2 {
		t.Fatalf("workers len = %d, want 2", len(resp.Workers))
	}
	if resp.Workers[0].Name != "cleanup_queue" {
		t.Errorf("first worker name = %q, want cleanup_queue", resp.Workers[0].Name)
	}
	if resp.Workers[1].LastError != "boom" {
		t.Errorf("second worker LastError = %q, want boom", resp.Workers[1].LastError)
	}
	if resp.Workers[1].ConsecutiveFailures != 3 {
		t.Errorf("second worker ConsecutiveFailures = %d, want 3", resp.Workers[1].ConsecutiveFailures)
	}
}

// TestHandleWorkers_NotWired covers the proxy-only-mode path: when
// the admin handler was constructed without a lifecycle manager, the
// route surfaces 503 so operators can tell "no worker pool here" from
// "worker pool is healthy but reporting nothing".
func TestHandleWorkers_NotWired(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t) // workerHealth deliberately nil
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/admin/api/workers", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleWorkers_RequiresToken ensures the worker-health endpoint
// is gated by the admin token like every other admin route.
func TestHandleWorkers_RequiresToken(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t)
	h.workerHealth = func() []adminapi.WorkerHealth { return nil }
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/admin/api/workers", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}
