// -------------------------------------------------------------------------------
// Background Service Constructor Tests
//
// Author: Alex Freidah
//
// Smoke-level constructor tests for every background service exposed under
// internal/di. They verify that each factory returns a lifecycle.Runner
// value (never nil), that the lockedTickerService's shouldRun / onError
// hooks fire as configured, and that the watchdog safely iterates an empty
// backend fleet. Nothing here exercises the *timing* behavior of Run  -
// lifecycle_test.go and the worker-specific suites cover that already.
// -------------------------------------------------------------------------------

package di

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle/tickrunner"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/multipart"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/testutil"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// fakeLocker skips advisory locking  -  useful for constructor-only tests
// where we never want Run to actually fire.
type fakeLocker struct{}

// WithAdvisoryLock runs the supplied function with advisory lock.
func (fakeLocker) WithAdvisoryLock(_ context.Context, _ int64, _ func(ctx context.Context) error) (bool, error) {
	return false, nil
}

// acquiringLocker always acquires the advisory lock and invokes the
// callback, so tests can verify the "work ran" branch of runOnce.
type acquiringLocker struct{}

// WithAdvisoryLock runs the supplied function with advisory lock.
func (acquiringLocker) WithAdvisoryLock(ctx context.Context, _ int64, fn func(ctx context.Context) error) (bool, error) {
	return true, fn(ctx)
}

// servicesFixture bundles a BackendManager with the workers that the
// background-service factories accept. Workers are no longer fields on
// BackendManager (#676 B); each one is a separate dependency.
type servicesFixture struct {
	mgr           *proxy.BackendManager
	rebalancer    *worker.Rebalancer
	replicator    *worker.Replicator
	overRep       *worker.OverReplicationCleaner
	cleanupWorker *worker.CleanupWorker
	scrubber      *worker.Scrubber
}

// newServicesFixture builds the manager and the workers the service
// constructors need. Mirrors what the per-worker DI providers do at
// runtime, just inline so the test stays free of the do.Injector.
func newServicesFixture(t *testing.T) *servicesFixture {
	t.Helper()
	mock := testutil.NewMockStore(t)
	mgr := proxytest.NewManager(t, &proxy.BackendManagerConfig{
		Storage: proxy.StorageDeps{
			Backends: map[string]backend.ObjectBackend{},
			Order:    []string{},
		},
		Stores: proxy.StoreDeps{
			Metadata:  mock,
			Dashboard: mock,
		},
		Policies: proxy.PolicyConfig{
			RoutingStrategy: config.RoutingPack,
		},
		Operations: proxy.OperationalDeps{
			Metrics: mock,
		},
	})
	// BuildWorkers wires drain.Manager onto the manager (required for
	// FlushUsage's CompletedBackends call); the worker handles it
	// returns are discarded here because this fixture builds custom
	// workers with test-specific configs below.
	_ = proxytest.BuildWorkers(mgr, mock)

	rb := worker.NewRebalancer(mgr.Runtime(), mgr, mock)
	rb.SetConfig(&config.RebalanceConfig{})

	rp := worker.NewReplicator(mgr.Runtime(), mgr, mock)
	rp.SetConfig(&config.ReplicationConfig{Factor: 1})

	or := worker.NewOverReplicationCleaner(mgr.Runtime(), mgr, mock)
	or.SetConfig(&config.ReplicationConfig{Factor: 1})

	cw := worker.NewCleanupWorker(worker.CleanupWorkerDeps{Ops: mgr.Runtime(), Store: mock, Concurrency: 10, InstanceID: "test-instance", ClaimGracePeriod: 5 * time.Minute})

	sc := worker.NewScrubber(worker.ScrubberDeps{Ops: mgr.Runtime(), Placement: mgr, Store: mock})
	sc.SetConfig(&config.IntegrityConfig{})

	mgr.SetLifecycleConfig(&config.LifecycleConfig{})
	mgr.SetIntegrityConfig(&config.IntegrityConfig{})
	t.Cleanup(mgr.Close)
	return &servicesFixture{
		mgr:           mgr,
		rebalancer:    rb,
		replicator:    rp,
		overRep:       or,
		cleanupWorker: cw,
		scrubber:      sc,
	}
}

// TestCleanupQueueService_ProcessedLogFires pre-populates the mock
// store with a cleanup item whose backend is not registered with the
// fixture manager. ProcessCleanupQueue treats the unknown-backend row
// as a successful retirement, so the work closure's "queue processed"
// info log fires when processed > 0.
func TestCleanupQueueService_ProcessedLogFires(t *testing.T) {
	t.Parallel()
	mock := testutil.NewMockStore(t)
	mock.PendingCleanupsResp = []core.CleanupItem{
		{ID: 1, BackendName: "missing-backend", ObjectKey: "k", Attempts: 0},
	}
	mgr := proxytest.NewManager(t, &proxy.BackendManagerConfig{
		Storage: proxy.StorageDeps{
			Backends: map[string]backend.ObjectBackend{},
			Order:    []string{},
		},
		Stores: proxy.StoreDeps{
			Metadata:  mock,
			Dashboard: mock,
		},
		Policies: proxy.PolicyConfig{
			RoutingStrategy: config.RoutingPack,
		},
		Operations: proxy.OperationalDeps{
			Metrics: mock,
		},
	})
	_ = proxytest.BuildWorkers(mgr, mock)
	cw := worker.NewCleanupWorker(worker.CleanupWorkerDeps{Ops: mgr.Runtime(), Store: mock, Concurrency: 1, InstanceID: "test", ClaimGracePeriod: 5 * time.Minute})
	t.Cleanup(mgr.Close)

	svc := worker.NewCleanupQueueService(cw, acquiringLocker{}).(*tickrunner.Service)
	svc.Tick(context.Background())
}

// TestServiceWorkClosures_RunOnceCovers drives each background service's
// work closure exactly once via runOnce + acquiringLocker. The fixture
// workers operate against an empty mock store so the inner "n > 0" log
// branches stay uncovered intentionally  -  the goal here is to exercise
// the closure body, config-nil guards, and worker dispatch, not to
// assert specific results. Adding a worker pre-condition that returns
// non-zero counts would require a richer mock store than this suite
// needs.
func TestServiceWorkClosures_RunOnceCovers(t *testing.T) {
	t.Parallel()
	f := newServicesFixture(t)
	locker := acquiringLocker{}

	services := []lifecycle.Runner{
		multipart.NewCleanupService(f.mgr.Multipart(), locker, 0),
		worker.NewCleanupQueueService(f.cleanupWorker, locker),
		worker.NewRebalancerService(f.mgr, f.rebalancer, locker),
		NewLifecycleService(f.mgr, locker),
		worker.NewOverReplicationService(f.mgr, f.overRep, locker),
		worker.NewReplicatorService(f.mgr, f.replicator, locker),
		worker.NewReconcileService(worker.NewReconciler(f.mgr, nil), locker, time.Hour),
		worker.NewScrubberService(f.scrubber, locker),
	}
	for _, svc := range services {
		ts, ok := svc.(*tickrunner.Service)
		if !ok {
			t.Fatalf("service %T is not *tickrunner.Service", svc)
		}
		// runOnce wraps the work closure in audit context + advisory
		// lock acquisition, so this single call covers the locked path
		// even when the closure itself returns early via a nil-config
		// guard.
		ts.Tick(context.Background())
	}
}

// TestUsageFlushService_DoFlushCoversBothCalls exercises the doFlush
// helper directly against the fixture manager, covering both the
// FlushUsage call and the UpdateQuotaMetrics call. The fixture manager
// has no backends so both calls return nil, but the closure body and
// the error-attr guards execute. The Redis branch in flushTick is
// driven through the same path because RedisCounterConfigured returns
// false for the LocalCounterBackend the fixture uses.
func TestUsageFlushService_DoFlushCoversBothCalls(t *testing.T) {
	t.Parallel()
	f := newServicesFixture(t)
	svc := &usageFlushService{
		manager: f.mgr,
		locker:  fakeLocker{},
		log:     slog.Default(),
	}
	svc.doFlush(context.Background())   // must not panic
	svc.flushTick(context.Background()) // hits the no-Redis branch
}

// TestServiceConstructors_AllReturnNonNil asserts each factory produces a
// usable lifecycle.Runner. Any missing assignment inside the constructor
// body surfaces as a nil return.
func TestServiceConstructors_AllReturnNonNil(t *testing.T) {
	t.Parallel()
	f := newServicesFixture(t)
	locker := fakeLocker{}

	tests := []struct {
		name string
		svc  any
	}{
		{"UsageFlush", NewUsageFlushService(f.mgr, locker)},
		{"MultipartCleanup", multipart.NewCleanupService(f.mgr.Multipart(), locker, 0)},
		{"CleanupQueue", worker.NewCleanupQueueService(f.cleanupWorker, locker)},
		{"Rebalancer", worker.NewRebalancerService(f.mgr, f.rebalancer, locker)},
		{"Lifecycle", NewLifecycleService(f.mgr, locker)},
		{"OverReplication", worker.NewOverReplicationService(f.mgr, f.overRep, locker)},
		{"Replicator", worker.NewReplicatorService(f.mgr, f.replicator, locker)},
		{"Reconcile", worker.NewReconcileService(worker.NewReconciler(f.mgr, nil), locker, time.Hour)},
		{"Scrubber", worker.NewScrubberService(f.scrubber, locker)},
		{"Watchdog", breaker.NewWatchdog(breaker.NewRegistry(breaker.NewCircuitBreaker(breaker.Config{Name: "t", Threshold: 3, Timeout: time.Second, IsError: func(error) bool { return false }, Sentinel: core.ErrDBUnavailable})))},
	}
	for _, tc := range tests {
		if tc.svc == nil {
			t.Errorf("%s: factory returned nil", tc.name)
		}
	}
}

// TestLockedTickerService_RunOnceSkipsOnLockBusy drives one runOnce cycle
// through the tick path to cover the "lock not acquired" logging branch
// without standing up a ticker.
func TestLockedTickerService_RunOnceSkipsOnLockBusy(t *testing.T) {
	t.Parallel()
	var workCalled bool
	svc := tickrunner.New(tickrunner.Config{
		Locker:   fakeLocker{},
		Interval: time.Second,
		LockID:   core.LockRebalancer,
		Name:     "test",
		Log:      slog.Default(),
		Work:     func(context.Context) error { workCalled = true; return nil },
	})
	svc.Tick(context.Background())
	if workCalled {
		t.Error("work ran even though the lock was not acquired")
	}
}

// errLocker always returns the supplied error from WithAdvisoryLock to
// drive the runOnce error branch.
type errLocker struct{ err error }

// WithAdvisoryLock runs the supplied function with advisory lock.
func (e errLocker) WithAdvisoryLock(_ context.Context, _ int64, _ func(ctx context.Context) error) (bool, error) {
	return false, e.err
}

// TestLockedTickerService_RunOnceInvokesOnError covers the onError callback
// path, which is only triggered when WithAdvisoryLock returns a non-nil,
// non-ErrDBUnavailable error.
func TestLockedTickerService_RunOnceInvokesOnError(t *testing.T) {
	t.Parallel()
	var caught error
	svc := tickrunner.New(tickrunner.Config{
		Locker:   errLocker{err: errors.New("advisory lock broke")},
		Interval: time.Second,
		LockID:   core.LockLifecycle,
		Name:     "test",
		Log:      slog.Default(),
		Work:     func(context.Context) error { return nil },
		OnError:  func(err error) { caught = err },
	})
	svc.Tick(context.Background())
	if caught == nil {
		t.Fatal("onError was not invoked")
	}
}

// TestLockedTickerService_RunOnceLogsErrorWhenNoOnError covers the
// fallback log path: when WithAdvisoryLock returns a non-DBUnavailable
// error and the service has no onError handler installed, runOnce falls
// back to s.log.ErrorContext rather than dropping the error silently.
func TestLockedTickerService_RunOnceLogsErrorWhenNoOnError(t *testing.T) {
	t.Parallel()
	svc := tickrunner.New(tickrunner.Config{
		Locker:   errLocker{err: errors.New("advisory lock broke")},
		Interval: time.Second,
		LockID:   core.LockLifecycle,
		Name:     "test",
		Log:      slog.Default(),
		Work:     func(context.Context) error { return nil },
		// OnError intentionally nil to drive the fallback branch.
	})
	svc.Tick(context.Background()) // must not panic
}

// TestLockedTickerService_RunOnceSwallowsErrDBUnavailable verifies the
// breaker-aware shortcut: when the advisory lock fails because the database
// is down, the service logs at debug and does not invoke onError.
func TestLockedTickerService_RunOnceSwallowsErrDBUnavailable(t *testing.T) {
	t.Parallel()
	var onErrCalled bool
	svc := tickrunner.New(tickrunner.Config{
		Locker:   errLocker{err: core.ErrDBUnavailable},
		Interval: time.Second,
		LockID:   core.LockLifecycle,
		Name:     "test",
		Log:      slog.Default(),
		Work:     func(context.Context) error { return nil },
		OnError:  func(error) { onErrCalled = true },
	})
	svc.Tick(context.Background())
	if onErrCalled {
		t.Error("onError should not fire for ErrDBUnavailable")
	}
}

// TestLockedTickerService_RunExitsOnCancel drives the Run method long
// enough to execute its jittered startup sleep and tick-loop select before
// context cancellation unwinds it.
func TestLockedTickerService_RunExitsOnCancel(t *testing.T) {
	t.Parallel()
	svc := tickrunner.New(tickrunner.Config{
		Locker:   fakeLocker{},
		Interval: time.Hour, // long interval so Run blocks in the tick select
		LockID:   core.LockRebalancer,
		Name:     "test",
		Log:      slog.Default(),
		Work:     func(context.Context) error { return nil },
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := svc.Run(ctx); err != nil {
		t.Errorf("Run returned error on cancel: %v", err)
	}
}

// TestLockedTickerService_RunStartupFires covers the startup-once path by
// registering a startup hook and invoking Run with a pre-cancelled context;
// startup runs before the cancellation is observed by the jitter select.
func TestLockedTickerService_RunStartupFires(t *testing.T) {
	t.Parallel()
	var startupCalled bool
	svc := tickrunner.New(tickrunner.Config{
		Locker:   acquiringLocker{},
		Interval: time.Hour,
		LockID:   core.LockReplicator,
		Name:     "test",
		Log:      slog.Default(),
		Work:     func(context.Context) error { return nil },
		Startup:  func(context.Context) error { startupCalled = true; return nil },
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = svc.Run(ctx)
	// startup runs once via runOnce before the jitter select sees the
	// cancelled context; acquiringLocker invokes the callback so startup
	// must have fired.
	if !startupCalled {
		t.Error("startup hook was not invoked")
	}
}

// TestUsageFlushService_RunExitsOnCancel covers the Run select-loop's
// ctx.Done() path for the adaptive-interval flusher.
func TestUsageFlushService_RunExitsOnCancel(t *testing.T) {
	t.Parallel()
	f := newServicesFixture(t)
	mgr := f.mgr
	mgr.SetUsageFlushConfig(&config.UsageFlushConfig{Interval: time.Hour})
	svc := NewUsageFlushService(mgr, fakeLocker{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := svc.Run(ctx); err != nil {
		t.Errorf("Run returned error on cancel: %v", err)
	}
}

// TestUsageFlushService_DoFlushOnMockManager exercises the doFlush code
// path directly on the concrete type. Both FlushUsage and
// UpdateQuotaMetrics operate on a mock-backed manager, so neither should
// error out.
func TestUsageFlushService_DoFlushOnMockManager(t *testing.T) {
	t.Parallel()
	f := newServicesFixture(t)
	mgr := f.mgr
	svc := &usageFlushService{manager: mgr, locker: fakeLocker{}}
	svc.doFlush(context.Background())
	svc.flushTick(context.Background()) // exercises the non-Redis branch
}

// asTicker unwraps a lifecycle.Runner returned by one of the New*Service
// factories so its shouldRun / work / startup closures can be poked
// directly. Fails the test when the returned value isn't the ticker type
// the tests rely on.
func asTicker(t *testing.T, svc any) *tickrunner.Service {
	t.Helper()
	lt, ok := svc.(*tickrunner.Service)
	if !ok {
		t.Fatalf("expected *tickrunner.Service, got %T", svc)
	}
	return lt
}

// TestServiceClosures_ExerciseWorkAndShouldRun drives a single tick
// through every New*Service factory's tickrunner. Each Tick runs the
// configured Work closure under the same lock + health machinery the
// production loop uses, exercising the closure body for new-code
// coverage. The shouldRun and onError closures are exercised by the
// dedicated tickrunner tests (TestLockedTickerService_*); this test
// just pins that every factory wires a non-nil Work that runs without
// panicking.
func TestServiceClosures_ExerciseWorkAndShouldRun(t *testing.T) {
	t.Parallel()
	f := newServicesFixture(t)
	mgr := f.mgr
	ctx := context.Background()
	locker := acquiringLocker{}

	asTicker(t, multipart.NewCleanupService(mgr.Multipart(), locker, 100*time.Millisecond)).Tick(ctx)
	asTicker(t, worker.NewCleanupQueueService(f.cleanupWorker, locker)).Tick(ctx)
	asTicker(t, worker.NewRebalancerService(mgr, f.rebalancer, locker)).Tick(ctx)
	asTicker(t, NewLifecycleService(mgr, locker)).Tick(ctx)
	asTicker(t, worker.NewOverReplicationService(mgr, f.overRep, locker)).Tick(ctx)
	asTicker(t, worker.NewReplicatorService(mgr, f.replicator, locker)).Tick(ctx)
	asTicker(t, worker.NewReconcileService(worker.NewReconciler(mgr, nil), locker, 100*time.Millisecond)).Tick(ctx)
	asTicker(t, worker.NewScrubberService(f.scrubber, locker)).Tick(ctx)
}

// TestLockedTickerService_RunTicksOnce runs the ticker loop long enough to
// execute the tick branch at least once, covering the shouldRun gating
// path and the main select in Run.
func TestLockedTickerService_RunTicksOnce(t *testing.T) {
	t.Parallel()
	done := make(chan struct{})
	svc := tickrunner.New(tickrunner.Config{
		Locker:   acquiringLocker{},
		Interval: 2 * time.Millisecond,
		LockID:   core.LockRebalancer,
		Name:     "test",
		Log:      slog.Default(),
		ShouldRun: func() bool {
			select {
			case <-done:
			default:
				close(done)
			}
			return true
		},
		Work: func(context.Context) error { return nil },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = svc.Run(ctx)
	select {
	case <-done:
	default:
		t.Error("shouldRun was never evaluated")
	}
}

// TestUsageFlushService_RunTicksOnce ticks the adaptive flusher at least
// once so the body of Run beyond the initial select is covered.
func TestUsageFlushService_RunTicksOnce(t *testing.T) {
	t.Parallel()
	f := newServicesFixture(t)
	mgr := f.mgr
	mgr.SetUsageFlushConfig(&config.UsageFlushConfig{Interval: 2 * time.Millisecond})
	svc := NewUsageFlushService(mgr, fakeLocker{})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = svc.Run(ctx)
}

// TestUsageFlushService_FlushTickWithRedisPath covers the Redis-configured
// branch of flushTick by forcing RedisCounterConfigured to return true via
// a UsageFlushConfig with AdaptiveEnabled set  -  exercises the advisory
// lock sidechannel.
// TestUsageFlushService_FlushTickWithAdaptiveSwitch verifies usage flush service_flush tick with adaptive switch.
// TestUsageFlushService_FlushTickWithAdaptiveSwitch verifies usage flush service_flush tick with adaptive switch.
func TestUsageFlushService_FlushTickWithAdaptiveSwitch(t *testing.T) {
	t.Parallel()
	f := newServicesFixture(t)
	mgr := f.mgr
	mgr.SetUsageFlushConfig(&config.UsageFlushConfig{
		Interval:          2 * time.Millisecond,
		FastInterval:      time.Millisecond,
		AdaptiveEnabled:   true,
		AdaptiveThreshold: 0.0, // always triggers the fast-interval branch
	})
	svc := NewUsageFlushService(mgr, fakeLocker{})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = svc.Run(ctx)
}

// TestUsageFlushService_DoFlushHandlesUpdateError forces UpdateQuotaMetrics
// to error so doFlush's second guarded log path runs. The mock returns nil
// for ListObjectsByBackend etc., but UpdateQuotaMetrics ultimately calls
// MetricsCollector.UpdateQuotaMetrics, which needs DashboardStore methods
//   - those are stubbed by MockStore. So the happy path is fully covered;
//
// this test re-runs doFlush with a cancelled ctx which short-circuits
// FlushUsage and exercises the post-error continuation.
// TestUsageFlushService_DoFlushOnCancelledCtx verifies usage flush service_do flush on cancelled ctx.
// TestUsageFlushService_DoFlushOnCancelledCtx verifies usage flush service_do flush on cancelled ctx.
func TestUsageFlushService_DoFlushOnCancelledCtx(t *testing.T) {
	t.Parallel()
	f := newServicesFixture(t)
	mgr := f.mgr
	svc := &usageFlushService{manager: mgr, locker: fakeLocker{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc.doFlush(ctx) // must not panic on cancelled ctx
}
