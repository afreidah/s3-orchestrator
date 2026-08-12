// -------------------------------------------------------------------------------
// Admin Handler - Worker Stubs
//
// Author: Alex Freidah
//
// Constructors that dress the generated ops mocks in the behaviour the handler
// tests need: a worker that reports N units of work, replays them through the
// progress observer, or fails. The mocks themselves are generated from the
// consumer-defined interfaces in deps.go - what lives here is only the
// per-test wiring, so a test reads as one line of intent rather than a
// hand-rolled type.
//
// Every stub is permissive (.AnyTimes()) on purpose: these are the
// collaborators a handler test drives past, not the thing it asserts on. A
// test that does care how a worker was called states its own expectation on
// the returned mock.
// -------------------------------------------------------------------------------

package admin

import (
	"context"
	"fmt"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/proxy/dashboard"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// trackN reports n successful units through the observer, so a streaming
// endpoint has progress lines to render.
func trackN(observer progress.Observer, n int, label func(int) string) {
	for i := range n {
		progress.Track(observer, label(i), func() string { return progress.StatusOK })
	}
}

// fixedKey labels every tracked unit identically, for tests that assert on the
// count rather than on which object moved.
func fixedKey(string) func(int) string {
	return func(int) string { return "fake-key" }
}

// backendOpsStub configures the BackendOps mock.
type backendOpsStub struct {
	flushErr     error
	integrity    *config.IntegrityConfig
	reconcileMap map[string]int64
	reconcileErr error
}

// newBackendOps builds a permissive BackendOps mock from cfg.
func newBackendOps(t *testing.T, cfg backendOpsStub) *MockBackendOps {
	t.Helper()
	m := NewMockBackendOps(gomock.NewController(t))
	m.EXPECT().FlushUsage(gomock.Any()).Return(cfg.flushErr).AnyTimes()
	m.EXPECT().ReconcileUsage(gomock.Any()).Return(cfg.reconcileMap, cfg.reconcileErr).AnyTimes()
	m.EXPECT().RecordUsage(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
	m.EXPECT().IntegrityConfig().Return(cfg.integrity).AnyTimes()
	return m
}

// newDashboardOps builds a permissive DashboardReader mock returning data or err.
func newDashboardOps(t *testing.T, data *dashboard.Data, err error) *MockDashboardReader {
	t.Helper()
	m := NewMockDashboardReader(gomock.NewController(t))
	m.EXPECT().GetData(gomock.Any()).Return(data, err).AnyTimes()
	return m
}

// newRuntimeOps builds a RuntimeOps mock whose GetBackend always misses, which
// is the branch the rewrite tests exercise, and whose metrics update succeeds.
func newRuntimeOps(t *testing.T) *MockRuntimeOps {
	t.Helper()
	m := NewMockRuntimeOps(gomock.NewController(t))
	m.EXPECT().GetBackend(gomock.Any()).
		Return(nil, fmt.Errorf("no backend")).AnyTimes()
	m.EXPECT().UpdateQuotaMetrics(gomock.Any()).Return(nil).AnyTimes()
	return m
}

// replicatorStub configures the ReplicatorOps mock.
type replicatorStub struct {
	cfg     *config.ReplicationConfig
	created int
	err     error
}

// newReplicator builds a ReplicatorOps mock that reports cfg.created copies.
func newReplicator(t *testing.T, cfg replicatorStub) *MockReplicatorOps {
	t.Helper()
	m := NewMockReplicatorOps(gomock.NewController(t))
	m.EXPECT().Config().Return(cfg.cfg).AnyTimes()
	m.EXPECT().Replicate(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ config.ReplicationConfig, observer progress.Observer) (int, error) {
			trackN(observer, cfg.created, fixedKey(""))
			return cfg.created, cfg.err
		}).AnyTimes()
	return m
}

// rebalancerStub configures the RebalancerOps mock. gotCfg captures the config
// the handler ran with, so the default-fallback can be asserted.
type rebalancerStub struct {
	cfg    *config.RebalanceConfig
	moved  int
	skip   string
	err    error
	gotCfg *config.RebalanceConfig
}

// newRebalancer builds a RebalancerOps mock that reports cfg.moved moves, or
// the configured skip reason.
func newRebalancer(t *testing.T, cfg *rebalancerStub) *MockRebalancerOps {
	t.Helper()
	m := NewMockRebalancerOps(gomock.NewController(t))
	m.EXPECT().Config().Return(cfg.cfg).AnyTimes()
	m.EXPECT().Rebalance(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, ran config.RebalanceConfig, observer progress.Observer) (worker.RebalanceSummary, error) {
			cfg.gotCfg = &ran
			if cfg.skip != "" {
				return worker.RebalanceSummary{SkipReason: cfg.skip}, cfg.err
			}
			trackN(observer, cfg.moved, func(i int) string { return fmt.Sprintf("obj-%d  src -> dst", i) })
			return worker.RebalanceSummary{WorkSummary: worker.WorkSummary{Succeeded: cfg.moved}}, cfg.err
		}).AnyTimes()
	return m
}

// overRepStub configures the OverReplicationOps mock.
type overRepStub struct {
	cfg      *config.ReplicationConfig
	count    int64
	countErr error
	cleaned  int
	cleanErr error
}

// newOverRep builds an OverReplicationOps mock reporting cfg.count pending and
// cleaning cfg.cleaned copies.
func newOverRep(t *testing.T, cfg overRepStub) *MockOverReplicationOps {
	t.Helper()
	m := NewMockOverReplicationOps(gomock.NewController(t))
	m.EXPECT().Config().Return(cfg.cfg).AnyTimes()
	m.EXPECT().CountPending(gomock.Any(), gomock.Any()).Return(cfg.count, cfg.countErr).AnyTimes()
	m.EXPECT().Clean(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ config.ReplicationConfig, observer progress.Observer) (int, error) {
			trackN(observer, cfg.cleaned, fixedKey(""))
			return cfg.cleaned, cfg.cleanErr
		}).AnyTimes()
	return m
}

// scrubberStub configures the ScrubberOps mock. backfillMore keeps reporting a
// further batch, so the handler's paging loop can be driven past one pass.
type scrubberStub struct {
	scrubChecked      int
	scrubFailed       int
	scrubSkipped      int
	scrubDeferred     int
	backfillProcessed int
	backfillMore      bool
	backfillCalls     int
}

// newScrubber builds a ScrubberOps mock from cfg.
func newScrubber(t *testing.T, cfg *scrubberStub) *MockScrubberOps {
	t.Helper()
	m := NewMockScrubberOps(gomock.NewController(t))
	m.EXPECT().Scrub(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ int, observer progress.Observer) worker.WorkSummary {
			trackN(observer, cfg.scrubChecked, fixedKey(""))
			return worker.WorkSummary{
				Attempted: cfg.scrubChecked,
				Succeeded: cfg.scrubChecked - cfg.scrubFailed,
				Failed:    cfg.scrubFailed,
				Skipped:   cfg.scrubSkipped,
				Deferred:  cfg.scrubDeferred,
			}
		}).AnyTimes()
	m.EXPECT().Backfill(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, batchSize, offset int, observer progress.Observer) (worker.WorkSummary, int) {
			cfg.backfillCalls++
			trackN(observer, cfg.backfillProcessed, fixedKey(""))
			sum := worker.WorkSummary{Attempted: cfg.backfillProcessed, Succeeded: cfg.backfillProcessed}
			if cfg.backfillMore {
				return sum, offset + batchSize
			}
			// One batch processed, then signal done with nextOffset=0.
			return sum, 0
		}).AnyTimes()
	return m
}

// newReconciler builds a Reconciler mock returning result or err from both the
// buffered and the streaming entry point.
func newReconciler(t *testing.T, result *worker.ReconcileResult, err error) *MockReconciler {
	t.Helper()
	m := NewMockReconciler(gomock.NewController(t))
	m.EXPECT().Reconcile(gomock.Any(), gomock.Any()).Return(result, err).AnyTimes()
	m.EXPECT().ReconcileStreaming(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, observer progress.Observer) (*worker.ReconcileResult, error) {
			if err != nil {
				return nil, err
			}
			progress.Track(observer, "fake-backend", func() string { return progress.StatusOK })
			return result, nil
		}).AnyTimes()
	return m
}
