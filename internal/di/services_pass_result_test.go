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

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle/tickrunner"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// passResultManager builds the smallest BackendRuntime that
// UpdateQuotaMetrics will accept. The mock store is permissive so the
// success path can call UpdateQuotaMetrics without erroring.
func passResultManager(t *testing.T) *infra.BackendRuntime {
	t.Helper()
	mock := storetest.NewMockMetadataStore(gomock.NewController(t))
	// The metrics collector runs on every quota refresh; stub its reads so the
	// test states only the pass-result behaviour it is about.
	expectCollectorReads(mock)
	return proxytest.NewRuntime(&proxytest.RuntimeOptions{
		Backends:        map[string]backend.ObjectBackend{},
		Order:           []string{},
		RoutingStrategy: config.RoutingPack,
		Metrics:         mock,
	})
}

// TestHandlePassResult_NilErrZeroCount covers the dominant "nothing
// happened this tick" case: no error, no work, no log line, no
// metrics refresh.
func TestHandlePassResult_NilErrZeroCount(t *testing.T) {
	t.Parallel()
	mgr := passResultManager(t)
	if err := tickrunner.HandlePassResult(context.Background(), slog.Default(), mgr, 0, nil, "objects_moved"); err != nil {
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
	if err := tickrunner.HandlePassResult(context.Background(), slog.Default(), mgr, 7, nil, "copies_created"); err != nil {
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
	err := tickrunner.HandlePassResult(context.Background(), slog.Default(), mgr, 0, core.ErrDBUnavailable, "objects_moved")
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
	err := tickrunner.HandlePassResult(context.Background(), slog.Default(), mgr, 0, boom, "objects_moved")
	if !errors.Is(err, boom) {
		t.Errorf("expected boom propagated, got %v", err)
	}
}

// expectCollectorReads stubs the reads metrics.Collector performs on a quota
// refresh. They are incidental to what these tests assert, but stating them
// keeps the mock strict about everything else.
func expectCollectorReads(m *storetest.MockMetadataStore) {
	m.EXPECT().GetQuotaStats(gomock.Any()).Return(map[string]core.QuotaStat{}, nil).AnyTimes()
	m.EXPECT().GetObjectCounts(gomock.Any()).Return(map[string]int64{}, nil).AnyTimes()
	m.EXPECT().GetUnverifiedObjectCounts(gomock.Any()).Return(map[string]int64{}, nil).AnyTimes()
	m.EXPECT().GetActiveMultipartCounts(gomock.Any()).Return(map[string]int64{}, nil).AnyTimes()
	m.EXPECT().GetUsageForPeriod(gomock.Any(), gomock.Any()).Return(map[string]core.UsageStat{}, nil).AnyTimes()
	m.EXPECT().GetPoolUsageForPeriod(gomock.Any(), gomock.Any()).Return(map[string]core.PoolUsage{}, nil).AnyTimes()
	m.EXPECT().GetUnderReplicatedObjects(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	m.EXPECT().CountOverReplicatedObjects(gomock.Any(), gomock.Any()).Return(int64(0), nil).AnyTimes()
	m.EXPECT().CountUnencryptedLocations(gomock.Any()).Return(int64(0), nil).AnyTimes()
}
