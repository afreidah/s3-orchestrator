// -------------------------------------------------------------------------------
// handlePassResult Tests
//
// Author: Alex Freidah
//
// Direct coverage of the shared post-call helper used by the
// rebalance, over-replication, and replication workers. The three
// per-service closures collapse to a single line through this helper,
// so its branches are the single canonical place to cover:
//
//   - non-DB error returns from the underlying worker -> surfaced to
//     runOnce as a tick failure
//   - ErrDBUnavailable -> squelched (breaker-aware)
//   - count > 0 -> success log + quota metrics refresh
//   - count == 0 -> no-op
// -------------------------------------------------------------------------------

package di

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/testutil"
)

// passResultManager builds the smallest BackendManager that
// UpdateQuotaMetrics will accept. The mock store is permissive so the
// success path can call UpdateQuotaMetrics without erroring.
func passResultManager(t *testing.T) *proxy.BackendManager {
	t.Helper()
	mock := testutil.NewMockStore(t)
	mgr := proxytest.NewManager(t, &proxy.BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{},
		Stores:          mock,
		Dashboard:       mock,
		Metrics:         mock,
		Order:           []string{},
		RoutingStrategy: config.RoutingPack,
	})
	t.Cleanup(mgr.Close)
	return mgr
}

// TestHandlePassResult_NilErrZeroCount covers the dominant "nothing
// happened this tick" case: no error, no work, no log line, no
// metrics refresh.
func TestHandlePassResult_NilErrZeroCount(t *testing.T) {
	t.Parallel()
	mgr := passResultManager(t)
	if err := handlePassResult(context.Background(), slog.Default(), mgr, 0, nil, "objects_moved"); err != nil {
		t.Errorf("handlePassResult: %v", err)
	}
}

// TestHandlePassResult_NilErrPositiveCount covers the success-with-
// work path: log message fires and UpdateQuotaMetrics is invoked.
// The fixture manager has no backends so the quota refresh is a
// no-op  -  exercising the closure body, not the metrics math.
func TestHandlePassResult_NilErrPositiveCount(t *testing.T) {
	t.Parallel()
	mgr := passResultManager(t)
	if err := handlePassResult(context.Background(), slog.Default(), mgr, 7, nil, "copies_created"); err != nil {
		t.Errorf("handlePassResult: %v", err)
	}
}

// TestHandlePassResult_DBUnavailableSquelched covers the
// breaker-aware shortcut: a worker call that surfaces
// core.ErrDBUnavailable must not be treated as a tick failure  -  the
// breaker is already handling DB-side outages.
func TestHandlePassResult_DBUnavailableSquelched(t *testing.T) {
	t.Parallel()
	mgr := passResultManager(t)
	err := handlePassResult(context.Background(), slog.Default(), mgr, 0, core.ErrDBUnavailable, "objects_moved")
	if err != nil {
		t.Errorf("ErrDBUnavailable should be squelched, got %v", err)
	}
}

// TestHandlePassResult_OtherErrorSurfaces covers the failure path
// runOnce reads: any non-DB error propagates so the tick lands in the
// failure branch of recordHealth, increments consecutive_failures,
// and shows up in /admin/api/workers.
func TestHandlePassResult_OtherErrorSurfaces(t *testing.T) {
	t.Parallel()
	mgr := passResultManager(t)
	boom := errors.New("worker exploded")
	err := handlePassResult(context.Background(), slog.Default(), mgr, 0, boom, "objects_moved")
	if !errors.Is(err, boom) {
		t.Errorf("expected boom propagated, got %v", err)
	}
}
