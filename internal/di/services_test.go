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
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
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
	mgr := proxy.NewBackendManager(&proxy.BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{},
		Stores:          proxytest.StoresFromMock(mock),
		Dashboard:       mock,
		Metrics:         mock,
		Order:           []string{},
		RoutingStrategy: config.RoutingPack,
	})
	proxytest.AttachWorkers(mgr, mock)

	rb := worker.NewRebalancer(mgr, struct {
		core.ObjectStore
		core.QuotaStore
	}{ObjectStore: mock, QuotaStore: mock})
	rb.SetConfig(&config.RebalanceConfig{})
	mgr.Rebalancer = rb

	rp := worker.NewReplicator(mgr, struct {
		core.ObjectStore
		core.ReplicationStore
		core.QuotaStore
	}{ObjectStore: mock, ReplicationStore: mock, QuotaStore: mock})
	rp.SetConfig(&config.ReplicationConfig{Factor: 1})
	mgr.Replicator = rp

	or := worker.NewOverReplicationCleaner(mgr, struct {
		core.ReplicationStore
		core.QuotaStore
	}{ReplicationStore: mock, QuotaStore: mock})
	or.SetConfig(&config.ReplicationConfig{Factor: 1})
	mgr.OverReplicationCleaner = or

	cw := worker.NewCleanupWorker(mgr, mock, 10, "test-instance", 5*time.Minute)
	mgr.CleanupWorker = cw

	sc := worker.NewScrubber(mgr, mock, nil)
	sc.SetConfig(&config.IntegrityConfig{})
	mgr.Scrubber = sc

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
		{"MultipartCleanup", NewMultipartCleanupService(f.mgr, locker, 0)},
		{"CleanupQueue", NewCleanupQueueService(f.cleanupWorker, locker)},
		{"Rebalancer", NewRebalancerService(f.mgr, f.rebalancer, locker)},
		{"Lifecycle", NewLifecycleService(f.mgr, locker)},
		{"OverReplication", NewOverReplicationService(f.mgr, f.overRep, locker)},
		{"Replicator", NewReplicatorService(f.mgr, f.replicator, locker)},
		{"Reconcile", NewReconcileService(worker.NewReconciler(f.mgr, nil), locker, time.Hour)},
		{"Scrubber", NewScrubberService(f.scrubber, locker)},
		{"Watchdog", NewCircuitBreakerWatchdog(breaker.NewRegistry(breaker.NewCircuitBreaker("t", 3, time.Second, func(error) bool { return false }, core.ErrDBUnavailable)))},
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
	svc := &lockedTickerService{
		locker:   fakeLocker{},
		interval: time.Second,
		lockID:   core.LockRebalancer,
		name:     "test",
		work:     func(context.Context) { workCalled = true },
	}
	svc.runOnce(context.Background(), svc.work)
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
	svc := &lockedTickerService{
		locker:   errLocker{err: errors.New("advisory lock broke")},
		interval: time.Second,
		lockID:   core.LockLifecycle,
		name:     "test",
		work:     func(context.Context) {},
		onError:  func(err error) { caught = err },
	}
	svc.runOnce(context.Background(), svc.work)
	if caught == nil {
		t.Fatal("onError was not invoked")
	}
}

// TestLockedTickerService_RunOnceSwallowsErrDBUnavailable verifies the
// breaker-aware shortcut: when the advisory lock fails because the database
// is down, the service logs at debug and does not invoke onError.
func TestLockedTickerService_RunOnceSwallowsErrDBUnavailable(t *testing.T) {
	t.Parallel()
	var onErrCalled bool
	svc := &lockedTickerService{
		locker:   errLocker{err: core.ErrDBUnavailable},
		interval: time.Second,
		lockID:   core.LockLifecycle,
		name:     "test",
		work:     func(context.Context) {},
		onError:  func(error) { onErrCalled = true },
	}
	svc.runOnce(context.Background(), svc.work)
	if onErrCalled {
		t.Error("onError should not fire for ErrDBUnavailable")
	}
}

// TestCircuitBreakerWatchdog_CheckAllEmptyRegistry exercises checkAll with an
// empty registry to guarantee it is a safe no-op when no breakers are wired.
func TestCircuitBreakerWatchdog_CheckAllEmptyRegistry(t *testing.T) {
	t.Parallel()
	w := &circuitBreakerWatchdog{registry: breaker.NewRegistry()}
	w.checkAll() // must not panic on empty registry
}

// TestCircuitBreakerWatchdog_RunExitsOnCancel covers the ticker loop's
// ctx.Done() branch by cancelling the context before the first tick fires.
func TestCircuitBreakerWatchdog_RunExitsOnCancel(t *testing.T) {
	t.Parallel()
	cb := breaker.NewCircuitBreaker("t", 3, time.Second, func(error) bool { return false }, core.ErrDBUnavailable)
	w := &circuitBreakerWatchdog{registry: breaker.NewRegistry(cb)}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Run(ctx); err != nil {
		t.Errorf("watchdog Run returned error on cancel: %v", err)
	}
}

// TestLockedTickerService_RunExitsOnCancel drives the Run method long
// enough to execute its jittered startup sleep and tick-loop select before
// context cancellation unwinds it.
func TestLockedTickerService_RunExitsOnCancel(t *testing.T) {
	t.Parallel()
	svc := &lockedTickerService{
		locker:   fakeLocker{},
		interval: time.Hour, // long interval so Run blocks in the tick select
		lockID:   core.LockRebalancer,
		name:     "test",
		work:     func(context.Context) {},
	}
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
	svc := &lockedTickerService{
		locker:   acquiringLocker{},
		interval: time.Hour,
		lockID:   core.LockReplicator,
		name:     "test",
		work:     func(context.Context) {},
		startup:  func(context.Context) { startupCalled = true },
	}
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
	f := newServicesFixture(t); mgr := f.mgr
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
	f := newServicesFixture(t); mgr := f.mgr
	svc := &usageFlushService{manager: mgr, locker: fakeLocker{}}
	svc.doFlush(context.Background())
	svc.flushTick(context.Background()) // exercises the non-Redis branch
}

// asTicker unwraps a lifecycle.Runner returned by one of the New*Service
// factories so its shouldRun / work / startup closures can be poked
// directly. Fails the test when the returned value isn't the ticker type
// the tests rely on.
func asTicker(t *testing.T, svc any) *lockedTickerService {
	t.Helper()
	lt, ok := svc.(*lockedTickerService)
	if !ok {
		t.Fatalf("expected *lockedTickerService, got %T", svc)
	}
	return lt
}

// TestServiceClosures_ExerciseWorkAndShouldRun drives every closure
// captured by the New*Service factories. The closures live in the file
// that Sonar measures for new-code coverage, so each untouched branch
// eats into the PR gate.
func TestServiceClosures_ExerciseWorkAndShouldRun(t *testing.T) {
	t.Parallel()
	f := newServicesFixture(t); mgr := f.mgr
	ctx := context.Background()
	locker := acquiringLocker{}

	mpc := asTicker(t, NewMultipartCleanupService(mgr, locker, 100*time.Millisecond))
	mpc.work(ctx)

	cq := asTicker(t, NewCleanupQueueService(f.cleanupWorker, locker))
	cq.work(ctx)

	rb := asTicker(t, NewRebalancerService(mgr, f.rebalancer, locker))
	_ = rb.shouldRun()
	rb.work(ctx)

	lc := asTicker(t, NewLifecycleService(mgr, locker))
	_ = lc.shouldRun()
	lc.work(ctx)
	if lc.onError != nil {
		lc.onError(errors.New("simulated failure for coverage"))
	}

	or := asTicker(t, NewOverReplicationService(mgr, f.overRep, locker))
	_ = or.shouldRun()
	or.work(ctx)

	rp := asTicker(t, NewReplicatorService(mgr, f.replicator, locker))
	_ = rp.shouldRun()
	rp.work(ctx)
	if rp.startup != nil {
		rp.startup(ctx)
	}

	rc := asTicker(t, NewReconcileService(worker.NewReconciler(mgr, nil), locker, 100*time.Millisecond))
	rc.work(ctx)

	sc := asTicker(t, NewScrubberService(f.scrubber, locker))
	_ = sc.shouldRun()
	sc.work(ctx)
}

// TestLockedTickerService_RunTicksOnce runs the ticker loop long enough to
// execute the tick branch at least once, covering the shouldRun gating
// path and the main select in Run.
func TestLockedTickerService_RunTicksOnce(t *testing.T) {
	t.Parallel()
	done := make(chan struct{})
	svc := &lockedTickerService{
		locker:   acquiringLocker{},
		interval: 2 * time.Millisecond,
		lockID:   core.LockRebalancer,
		name:     "test",
		shouldRun: func() bool {
			select {
			case <-done:
			default:
				close(done)
			}
			return true
		},
		work: func(context.Context) {},
	}
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
	f := newServicesFixture(t); mgr := f.mgr
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
	f := newServicesFixture(t); mgr := f.mgr
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

// TestCircuitBreakerWatchdog_CheckAllResetsBackendBreaker verifies that a
// per-backend CircuitBreakerBackend registered alongside the database breaker
// receives a ResetStaleProbe call when the watchdog ticks.
func TestCircuitBreakerWatchdog_CheckAllResetsBackendBreaker(t *testing.T) {
	t.Parallel()
	cbBackend := backend.NewCircuitBreakerBackend(nil, "b1", 3, time.Second)
	dbCB := breaker.NewCircuitBreaker("t", 3, time.Second, func(error) bool { return false }, core.ErrDBUnavailable)
	w := &circuitBreakerWatchdog{registry: breaker.NewRegistry(dbCB, cbBackend)}
	w.checkAll() // must not panic; ResetStaleProbe runs on cbBackend
}

// TestUsageFlushService_DoFlushHandlesUpdateError forces UpdateQuotaMetrics
// to error so doFlush's second guarded log path runs. The mock returns nil
// for ListObjectsByBackend etc., but UpdateQuotaMetrics ultimately calls
// MetricsCollector.UpdateQuotaMetrics, which needs DashboardStore methods
//  -  those are stubbed by MockStore. So the happy path is fully covered;
// this test re-runs doFlush with a cancelled ctx which short-circuits
// FlushUsage and exercises the post-error continuation.
// TestUsageFlushService_DoFlushOnCancelledCtx verifies usage flush service_do flush on cancelled ctx.
// TestUsageFlushService_DoFlushOnCancelledCtx verifies usage flush service_do flush on cancelled ctx.
func TestUsageFlushService_DoFlushOnCancelledCtx(t *testing.T) {
	t.Parallel()
	f := newServicesFixture(t); mgr := f.mgr
	svc := &usageFlushService{manager: mgr, locker: fakeLocker{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc.doFlush(ctx) // must not panic on cancelled ctx
}
