// -------------------------------------------------------------------------------
// Admin /rebalance Handler Tests
//
// Author: Alex Freidah
//
// Covers the operator-facing surface of the on-demand rebalance endpoint:
//
//   - happy path: handler runs the rebalancer and returns the move count
//   - rebalancer not wired -> skipped with moved=0 (proxy-only deployments)
//   - rebalancer error -> 500
//   - auth path: missing token rejected like every other admin route
//
// The move-planning logic itself lives in internal/worker; here the contract
// is purely the HTTP response shape and the default-config fallback.
// -------------------------------------------------------------------------------

package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// TestHandleRebalance_HappyPath drives the success branch: the handler runs
// the rebalancer and surfaces the move count under "moved". With a nil worker
// config it must fall back to the spread-strategy defaults.
func TestHandleRebalance_HappyPath(t *testing.T) {
	t.Parallel()
	stub := &rebalancerStub{moved: 3}
	fake := newRebalancer(t, stub)
	h := newTestHandler(t)
	h.rebalancer = fake
	h.backendOps = newBackendOps(t, backendOpsStub{})
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/admin/api/rebalance", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp adminapi.RebalanceResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "ok" || resp.Moved != 3 {
		t.Errorf("got {status=%q moved=%d}, want {ok 3}", resp.Status, resp.Moved)
	}
	// Reason is omitted on the ok path so the two outcomes stay distinguishable.
	if resp.Reason != "" {
		t.Errorf("reason = %q, want empty on the ok path", resp.Reason)
	}
	// Defaults applied when the worker config is nil, mirroring the dashboard.
	if g := *stub.gotCfg; g.Strategy != defaultRebalanceStrategy || g.BatchSize != defaultRebalanceBatchSize ||
		g.Threshold != defaultRebalanceThreshold || g.Concurrency != defaultRebalanceConcurrency {
		t.Errorf("ran with cfg %+v, want spread defaults", g)
	}
}

// TestHandleRebalance_PreservesConfiguredStrategy verifies the operator's
// configured strategy is used verbatim and only zero-value fields are defaulted.
func TestHandleRebalance_PreservesConfiguredStrategy(t *testing.T) {
	t.Parallel()
	stub := &rebalancerStub{
		cfg:   &config.RebalanceConfig{Strategy: "pack", BatchSize: 50, Threshold: 0.2, Concurrency: 8},
		moved: 1,
	}
	h := newTestHandler(t)
	h.rebalancer = newRebalancer(t, stub)
	h.backendOps = newBackendOps(t, backendOpsStub{})
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/admin/api/rebalance", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	mux.ServeHTTP(httptest.NewRecorder(), req)

	if g := *stub.gotCfg; g.Strategy != "pack" || g.BatchSize != 50 || g.Threshold != 0.2 || g.Concurrency != 8 {
		t.Errorf("ran with cfg %+v, want configured pack values", g)
	}
}

// TestHandleRebalance_QuotaMetricsErrorStillOK verifies a post-move quota
// metrics refresh failure is logged but does not fail the rebalance: the move
// already happened, so the response is still ok with the move count.
func TestHandleRebalance_QuotaMetricsErrorStillOK(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t)
	h.rebalancer = newRebalancer(t, &rebalancerStub{moved: 2})
	h.backendOps = allFailingOps{} // UpdateQuotaMetrics returns an error
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/admin/api/rebalance", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp adminapi.RebalanceResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "ok" || resp.Moved != 2 {
		t.Errorf("got {status=%q moved=%d}, want {ok 2}", resp.Status, resp.Moved)
	}
}

// TestHandleRebalance_NotWired covers the proxy-only path: when the handler
// has no rebalancer the endpoint returns a skipped result with moved=0 rather
// than panicking.
func TestHandleRebalance_NotWired(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t) // rebalancer deliberately nil
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/admin/api/rebalance", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp adminapi.RebalanceResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "skipped" || resp.Moved != 0 {
		t.Errorf("got {status=%q moved=%d}, want {skipped 0}", resp.Status, resp.Moved)
	}
	// Reason is the skipped path's only explanation of why nothing moved.
	if resp.Reason == "" {
		t.Error("reason is empty, want an explanation on the skipped path")
	}
}

// TestHandleRebalance_SkippedCycle covers a cycle that planned no moves. It
// must report the reason rather than a zero move count, which reads as a run
// that found nothing to do.
func TestHandleRebalance_SkippedCycle(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t)
	h.rebalancer = newRebalancer(t, &rebalancerStub{skip: worker.SkipReasonWithinThreshold})
	h.backendOps = newBackendOps(t, backendOpsStub{})
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/admin/api/rebalance", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	var resp adminapi.RebalanceResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "skipped" || resp.Reason != worker.SkipReasonWithinThreshold {
		t.Errorf("got {status=%q reason=%q}, want the threshold skip", resp.Status, resp.Reason)
	}
}

// TestHandleRebalance_Error surfaces a 500 when the rebalancer fails.
func TestHandleRebalance_Error(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t)
	h.rebalancer = newRebalancer(t, &rebalancerStub{err: errors.New("boom")})
	h.backendOps = newBackendOps(t, backendOpsStub{})
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/admin/api/rebalance", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleRebalance_RequiresToken ensures the endpoint is gated by the admin
// token like every other admin route.
func TestHandleRebalance_RequiresToken(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t)
	h.rebalancer = newRebalancer(t, &rebalancerStub{})
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/admin/api/rebalance", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}
