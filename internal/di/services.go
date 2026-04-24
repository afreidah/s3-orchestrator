// -------------------------------------------------------------------------------
// Background Service Definitions
//
// Author: Alex Freidah
//
// Service types for the lifecycle manager. Each wraps a periodic background
// task behind the lifecycle.Service interface. Tasks that must not run
// concurrently across instances use PostgreSQL advisory locks via
// lockedTickerService. These live under internal/di because they are
// provider plumbing — the main binary never constructs them directly.
// -------------------------------------------------------------------------------

package di

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// Default intervals and thresholds for the background services below. Used
// when the corresponding config section is absent or leaves the field zero.
const (
	defaultUsageFlushInterval     = 30 * time.Second
	defaultMultipartStaleTimeout  = 24 * time.Hour
	defaultMultipartCleanupTick   = 1 * time.Hour
	defaultCleanupQueueTick       = 1 * time.Minute
	defaultRebalanceInterval      = 6 * time.Hour
	defaultLifecycleTick          = 1 * time.Hour
	defaultOverReplicationTick    = 5 * time.Minute
	defaultReplicatorTick         = 5 * time.Minute
	defaultCircuitBreakerWatchdog = 1 * time.Minute
	defaultScrubberInterval       = 6 * time.Hour
)

// lockedTickerService runs a function on a fixed interval under an advisory
// lock. It handles audit context creation, lock acquisition, skip/error
// logging, and context cancellation.
type lockedTickerService struct {
	locker   store.AdvisoryLocker
	interval time.Duration
	lockID   int64
	name     string

	shouldRun func() bool
	startup   func(ctx context.Context)
	work      func(ctx context.Context)
	onError   func(err error)
}

// Run implements lifecycle.Service with a jittered first tick to prevent
// thundering herd on the advisory lock at startup.
func (s *lockedTickerService) Run(ctx context.Context) error {
	if s.startup != nil {
		s.runOnce(ctx, s.startup)
	}

	jitter := rand.N(s.interval / 2) //nolint:gosec // G404: startup jitter does not require crypto-strength randomness
	select {
	case <-time.After(jitter):
	case <-ctx.Done():
		return nil
	}

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if s.shouldRun != nil && !s.shouldRun() {
				continue
			}
			s.runOnce(ctx, s.work)
		case <-ctx.Done():
			return nil
		}
	}
}

// runOnce creates an audit context, acquires the advisory lock, and runs fn.
func (s *lockedTickerService) runOnce(ctx context.Context, fn func(ctx context.Context)) {
	tickCtx := audit.WithRequestID(ctx, audit.NewID())
	acquired, err := s.locker.WithAdvisoryLock(tickCtx, s.lockID,
		func(lockCtx context.Context) error {
			fn(lockCtx)
			return nil
		})
	if err != nil && !errors.Is(err, store.ErrDBUnavailable) {
		if s.onError != nil {
			s.onError(err)
		} else {
			slog.ErrorContext(ctx, s.name+" failed", "error", err)
		}
	}
	if !acquired {
		slog.DebugContext(ctx, s.name+" skipped, another instance holds the lock")
	}
}

// -------------------------------------------------------------------------
// USAGE FLUSH (unique: no advisory lock, adaptive interval)
// -------------------------------------------------------------------------

// usageFlushService periodically flushes in-memory usage counters to the
// database, acquiring an advisory lock only when Redis counters are active.
type usageFlushService struct {
	manager *proxy.BackendManager
	locker  store.AdvisoryLocker
}

// NewUsageFlushService constructs the usage flush background service.
func NewUsageFlushService(manager *proxy.BackendManager, locker store.AdvisoryLocker) lifecycle.Service {
	return &usageFlushService{manager: manager, locker: locker}
}

// Run periodically flushes in-memory usage counters and adapts the tick
// interval toward FastInterval when a backend nears its limits.
func (s *usageFlushService) Run(ctx context.Context) error {
	cfg := s.manager.UsageFlushConfig()
	interval := defaultUsageFlushInterval
	if cfg != nil {
		interval = cfg.Interval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	currentInterval := interval

	for {
		select {
		case <-ticker.C:
			tickCtx := audit.WithRequestID(ctx, audit.NewID())
			s.flushTick(tickCtx)

			cfg = s.manager.UsageFlushConfig()
			if cfg != nil {
				targetInterval := cfg.Interval
				if cfg.AdaptiveEnabled && s.manager.NearUsageLimit(cfg.AdaptiveThreshold) {
					targetInterval = cfg.FastInterval
				}
				if targetInterval != currentInterval {
					ticker.Reset(targetInterval)
					currentInterval = targetInterval
					slog.InfoContext(ctx, "usage flush interval adjusted", "interval", targetInterval)
				}
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// flushTick runs a single flush+metrics cycle. When Redis counters are
// configured, wraps the flush in an advisory lock so only one instance
// performs the destructive GETSET.
func (s *usageFlushService) flushTick(ctx context.Context) {
	if s.manager.RedisCounterConfigured() {
		acquired, err := s.locker.WithAdvisoryLock(ctx, store.LockUsageFlush,
			func(lockCtx context.Context) error {
				s.doFlush(lockCtx)
				return nil
			})
		if err != nil && !errors.Is(err, store.ErrDBUnavailable) {
			slog.ErrorContext(ctx, "usage flush failed", "error", err)
		}
		if !acquired {
			slog.DebugContext(ctx, "usage flush skipped, another instance holds the lock")
		}
		return
	}
	s.doFlush(ctx)
}

// doFlush performs the actual flush and quota metric update.
func (s *usageFlushService) doFlush(ctx context.Context) {
	if err := s.manager.FlushUsage(ctx); err != nil && !errors.Is(err, store.ErrDBUnavailable) {
		slog.ErrorContext(ctx, "failed to flush usage counters", "error", err)
	}
	if err := s.manager.UpdateQuotaMetrics(ctx); err != nil && !errors.Is(err, store.ErrDBUnavailable) {
		slog.ErrorContext(ctx, "failed to update quota metrics", "error", err)
	}
}

// -------------------------------------------------------------------------
// SERVICE CONSTRUCTORS
// -------------------------------------------------------------------------

// NewMultipartCleanupService constructs the multipart-cleanup background service.
func NewMultipartCleanupService(manager *proxy.BackendManager, locker store.AdvisoryLocker, staleTimeout time.Duration) lifecycle.Service {
	if staleTimeout <= 0 {
		staleTimeout = defaultMultipartStaleTimeout
	}
	return &lockedTickerService{
		locker:   locker,
		interval: defaultMultipartCleanupTick,
		lockID:   store.LockMultipartCleanup,
		name:     "Multipart cleanup",
		work: func(ctx context.Context) {
			manager.MultipartManager.CleanupStaleMultipartUploads(ctx, staleTimeout)
		},
	}
}

// NewCleanupQueueService constructs the cleanup-queue background service.
func NewCleanupQueueService(manager *proxy.BackendManager, locker store.AdvisoryLocker) lifecycle.Service {
	return &lockedTickerService{
		locker:   locker,
		interval: defaultCleanupQueueTick,
		lockID:   store.LockCleanupQueue,
		name:     "Cleanup queue",
		work: func(ctx context.Context) {
			processed, failed := manager.CleanupWorker.ProcessCleanupQueue(ctx)
			if processed > 0 || failed > 0 {
				slog.InfoContext(ctx, "cleanup queue processed", "processed", processed, "failed", failed)
			}
		},
	}
}

// NewRebalancerService constructs the rebalancer background service.
func NewRebalancerService(manager *proxy.BackendManager, locker store.AdvisoryLocker) lifecycle.Service {
	interval := defaultRebalanceInterval
	if rcfg := manager.Rebalancer.Config(); rcfg != nil && rcfg.Interval > 0 {
		interval = rcfg.Interval
	}
	return &lockedTickerService{
		locker:   locker,
		interval: interval,
		lockID:   store.LockRebalancer,
		name:     "Rebalance",
		shouldRun: func() bool {
			rcfg := manager.Rebalancer.Config()
			return rcfg != nil && rcfg.Enabled
		},
		work: func(ctx context.Context) {
			rcfg := manager.Rebalancer.Config()
			if rcfg == nil {
				return
			}
			moved, err := manager.Rebalancer.Rebalance(ctx, *rcfg)
			if err != nil && !errors.Is(err, store.ErrDBUnavailable) {
				slog.ErrorContext(ctx, "rebalance failed", "error", err)
			} else if moved > 0 {
				slog.InfoContext(ctx, "rebalance completed", "objects_moved", moved)
				if err := manager.UpdateQuotaMetrics(ctx); err != nil {
					slog.WarnContext(ctx, "failed to update quota metrics after rebalance", "error", err)
				}
			}
		},
	}
}

// NewLifecycleService constructs the lifecycle-expiration background service.
func NewLifecycleService(manager *proxy.BackendManager, locker store.AdvisoryLocker) lifecycle.Service {
	return &lockedTickerService{
		locker:   locker,
		interval: defaultLifecycleTick,
		lockID:   store.LockLifecycle,
		name:     "Lifecycle",
		shouldRun: func() bool {
			cfg := manager.LifecycleConfig()
			return cfg != nil && len(cfg.Rules) > 0
		},
		work: func(ctx context.Context) {
			cfg := manager.LifecycleConfig()
			if cfg == nil {
				return
			}
			deleted, failed := manager.ProcessLifecycleRules(ctx, cfg.Rules)
			if deleted > 0 || failed > 0 {
				slog.InfoContext(ctx, "lifecycle expiration completed",
					"deleted", deleted, "failed", failed)
			}
			if failed > 0 {
				telemetry.LifecycleRunsTotal.WithLabelValues("partial").Inc()
			} else {
				telemetry.LifecycleRunsTotal.WithLabelValues("success").Inc()
			}
		},
		onError: func(err error) {
			slog.Error("Lifecycle expiration failed", "error", err) //nolint:sloglint // callback has no context
			telemetry.LifecycleRunsTotal.WithLabelValues("error").Inc()
		},
	}
}

// NewOverReplicationService constructs the over-replication cleanup service.
func NewOverReplicationService(manager *proxy.BackendManager, locker store.AdvisoryLocker) lifecycle.Service {
	interval := defaultOverReplicationTick
	if rcfg := manager.OverReplicationCleaner.Config(); rcfg != nil && rcfg.WorkerInterval > 0 {
		interval = rcfg.WorkerInterval
	}
	return &lockedTickerService{
		locker:   locker,
		interval: interval,
		lockID:   store.LockOverReplication,
		name:     "Over-replication cleanup",
		shouldRun: func() bool {
			rcfg := manager.OverReplicationCleaner.Config()
			return rcfg != nil && rcfg.Factor > 1
		},
		work: func(ctx context.Context) {
			rcfg := manager.OverReplicationCleaner.Config()
			if rcfg == nil {
				return
			}
			removed, err := manager.OverReplicationCleaner.Clean(ctx, *rcfg)
			if err != nil && !errors.Is(err, store.ErrDBUnavailable) {
				slog.ErrorContext(ctx, "over-replication cleanup failed", "error", err)
			} else if removed > 0 {
				slog.InfoContext(ctx, "over-replication cleanup completed", "copies_removed", removed)
				if err := manager.UpdateQuotaMetrics(ctx); err != nil {
					slog.WarnContext(ctx, "failed to update quota metrics after over-replication cleanup", "error", err)
				}
			}
		},
	}
}

// NewReplicatorService constructs the replication background service.
func NewReplicatorService(manager *proxy.BackendManager, locker store.AdvisoryLocker) lifecycle.Service {
	replicateWork := func(ctx context.Context) {
		rcfg := manager.Replicator.Config()
		if rcfg == nil {
			return
		}
		created, err := manager.Replicator.Replicate(ctx, *rcfg)
		if err != nil && !errors.Is(err, store.ErrDBUnavailable) {
			slog.ErrorContext(ctx, "replication failed", "error", err)
		} else if created > 0 {
			slog.InfoContext(ctx, "replication completed", "copies_created", created)
			if err := manager.UpdateQuotaMetrics(ctx); err != nil {
				slog.WarnContext(ctx, "failed to update quota metrics after replication", "error", err)
			}
		}
	}

	interval := defaultReplicatorTick
	if rcfg := manager.Replicator.Config(); rcfg != nil && rcfg.WorkerInterval > 0 {
		interval = rcfg.WorkerInterval
	}
	return &lockedTickerService{
		locker:   locker,
		interval: interval,
		lockID:   store.LockReplicator,
		name:     "Replication",
		shouldRun: func() bool {
			rcfg := manager.Replicator.Config()
			return rcfg != nil && rcfg.Factor > 1
		},
		startup: replicateWork,
		work:    replicateWork,
	}
}

// NewReconcileService constructs the reconcile background service.
func NewReconcileService(reconciler *worker.Reconciler, locker store.AdvisoryLocker, interval time.Duration) lifecycle.Service {
	return &lockedTickerService{
		locker:   locker,
		interval: interval,
		lockID:   store.LockReconcile,
		name:     "Reconcile",
		work: func(ctx context.Context) {
			reconciler.Run(ctx)
		},
	}
}

// -------------------------------------------------------------------------
// CIRCUIT BREAKER WATCHDOG
// -------------------------------------------------------------------------

// circuitBreakerWatchdog periodically checks all circuit breakers for stale
// half-open probes and resets them. This prevents circuits from getting
// stuck half-open indefinitely when no new requests arrive.
type circuitBreakerWatchdog struct {
	manager *proxy.BackendManager
	dbCB    *breaker.CircuitBreaker
}

// NewCircuitBreakerWatchdog constructs the watchdog background service.
func NewCircuitBreakerWatchdog(manager *proxy.BackendManager, dbCB *breaker.CircuitBreaker) lifecycle.Service {
	return &circuitBreakerWatchdog{manager: manager, dbCB: dbCB}
}

// Run implements lifecycle.Service. Checks every defaultCircuitBreakerWatchdog
// (1 minute) — half the breaker probe timeout.
func (w *circuitBreakerWatchdog) Run(ctx context.Context) error {
	ticker := time.NewTicker(defaultCircuitBreakerWatchdog)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			w.checkAll()
		}
	}
}

// checkAll iterates all circuit breakers and resets stale probes.
func (w *circuitBreakerWatchdog) checkAll() {
	w.dbCB.ResetStaleProbe()

	for _, be := range w.manager.Backends() {
		if cb, ok := be.(*backend.CircuitBreakerBackend); ok {
			cb.ResetStaleProbe()
		}
	}
}

// -------------------------------------------------------------------------
// SCRUBBER
// -------------------------------------------------------------------------

// NewScrubberService constructs the integrity scrubber background service.
func NewScrubberService(manager *proxy.BackendManager, locker store.AdvisoryLocker) lifecycle.Service {
	interval := defaultScrubberInterval
	if icfg := manager.Scrubber.Config(); icfg != nil && icfg.ScrubberInterval > 0 {
		interval = icfg.ScrubberInterval
	}
	return &lockedTickerService{
		locker:   locker,
		interval: interval,
		lockID:   store.LockScrubber,
		name:     "Scrubber",
		shouldRun: func() bool {
			icfg := manager.Scrubber.Config()
			return icfg != nil && icfg.Enabled && icfg.ScrubberInterval > 0
		},
		work: func(ctx context.Context) {
			icfg := manager.Scrubber.Config()
			if icfg == nil {
				return
			}
			checked, failed := manager.Scrubber.Scrub(ctx, icfg.ScrubberBatchSize)
			if checked > 0 || failed > 0 {
				slog.InfoContext(ctx, "scrubber completed", "checked", checked, "failed", failed)
			}
		},
	}
}