// -------------------------------------------------------------------------------
// Background Service Definitions
//
// Author: Alex Freidah
//
// Service types for the lifecycle manager. Each wraps a periodic background
// task behind the lifecycle.Runner interface. Tasks that must not run
// concurrently across instances use PostgreSQL advisory locks via
// lockedTickerService. These live under internal/di because they are
// provider plumbing  -  the main binary never constructs them directly.
// -------------------------------------------------------------------------------

package di

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// advisoryLocker is the consumer-defined slice of the metadata store
// the locked-ticker services need: a single TryAdvisoryLock call per
// tick. Declared here so this package owns its dependency contract
// instead of importing a producer-side type from internal/store/core.
type advisoryLocker interface {
	WithAdvisoryLock(ctx context.Context, lockID int64, fn func(ctx context.Context) error) (bool, error)
}

// Default intervals and thresholds for the background services below. Used
// when the corresponding config section is absent or leaves the field zero.
const (
	defaultUsageFlushInterval     = 30 * time.Second
	defaultMultipartStaleTimeout  = 24 * time.Hour
	defaultMultipartCleanupTick   = 1 * time.Hour
	defaultCleanupQueueTick       = 1 * time.Minute
	defaultPendingReaperTick      = 1 * time.Minute
	defaultRebalanceInterval      = 6 * time.Hour
	defaultLifecycleTick          = 1 * time.Hour
	defaultOverReplicationTick    = 5 * time.Minute
	defaultReplicatorTick         = 5 * time.Minute
	defaultCircuitBreakerWatchdog = 1 * time.Minute
	defaultScrubberInterval       = 6 * time.Hour
)

// Shared log messages for the periodic "Rebalance / Replication /
// Over-replication cleanup" services. The three work closures emit the
// same terminal events (pass failed, pass completed, quota metrics
// refresh failed after pass); the component attr on each scoped logger
// disambiguates which service produced the line.
const (
	msgPassFailed                = "pass failed"
	msgPassCompleted             = "pass completed"
	msgQuotaMetricsRefreshFailed = "quota metrics refresh failed after pass"
)

// lockedTickerService runs a function on a fixed interval under an advisory
// lock. It handles audit context creation, lock acquisition, skip/error
// logging, and context cancellation. The component identity lives on the
// scoped logger (Component attr) rather than in message text, so logs from
// every service share the same shape and operators filter by attribute.
//
// Per-service health state (lastSuccess, lastFailure, lastError,
// consecutiveFailures) is recorded after each tick so operators can
// query worker liveness through the admin endpoint and alert on
// staleness through Prometheus.
type lockedTickerService struct {
	locker   advisoryLocker
	interval time.Duration
	lockID   int64
	// name is the canonical snake_case component slug; it both names
	// the service in tests and seeds the scoped logger's component attr.
	name string
	// log is scoped to logfmt.Component(name) so every log line from
	// this service carries the component identity as an attr rather
	// than embedded in the message text.
	log *slog.Logger

	shouldRun func() bool
	startup   func(ctx context.Context) error
	work      func(ctx context.Context) error
	onError   func(err error)

	// health is the latest snapshot of per-service liveness, updated by
	// runOnce after every tick. Read by Health() under healthMu so the
	// admin endpoint sees consistent reads without blocking the tick.
	healthMu sync.Mutex
	health   lifecycle.WorkerHealth
}

// componentLogger returns the scoped logger every service uses,
// derived from the snake_case slug so the component attr is the single
// source of truth for log filtering. Callers also hold the slug as the
// service's name field.
func componentLogger(slug string) *slog.Logger {
	return slog.Default().With(logfmt.Component(slug))
}

// handlePassResult is the shared post-call handling for the three
// nearly-identical "pass" workers (rebalance, over-replication,
// replication). Each one returns (count, err) from a worker call and
// then either: surfaces non-DB errors as tick failures (so health
// reporting sees them), or  -  when work was done  -  logs a
// completion message with a work-specific count key and refreshes
// quota metrics so the dashboard reflects the move. Centralized so
// the three closures cannot drift, and so coverage of the error and
// count>0 branches lands in one place.
func handlePassResult(ctx context.Context, log *slog.Logger, manager *proxy.BackendManager, count int, err error, countKey string) error {
	if err != nil {
		if errors.Is(err, core.ErrDBUnavailable) {
			return nil
		}
		return err
	}
	if count > 0 {
		log.InfoContext(ctx, msgPassCompleted, countKey, count)
		if qerr := manager.UpdateQuotaMetrics(ctx); qerr != nil {
			log.WarnContext(ctx, msgQuotaMetricsRefreshFailed, "error", qerr)
		}
	}
	return nil
}

// Run implements lifecycle.Runner with a jittered first tick to prevent
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

// runOnce creates an audit context, acquires the advisory lock, runs
// fn, and records the resulting health state. Lock-busy and
// shouldRun-gated ticks are accounted as "skipped" so health metrics
// can distinguish them from outright failures.
func (s *lockedTickerService) runOnce(ctx context.Context, fn func(ctx context.Context) error) {
	tickCtx := audit.WithRequestID(ctx, audit.NewID())
	var workErr error
	acquired, lockErr := s.locker.WithAdvisoryLock(tickCtx, s.lockID,
		func(lockCtx context.Context) error {
			workErr = fn(lockCtx)
			return nil
		})

	switch {
	case lockErr != nil && !errors.Is(lockErr, core.ErrDBUnavailable):
		if s.onError != nil {
			s.onError(lockErr)
		} else {
			s.log.ErrorContext(ctx, "tick failed", "error", lockErr)
		}
		s.recordHealth(false, fmt.Errorf("advisory lock: %w", lockErr))
	case !acquired:
		s.log.DebugContext(ctx, "tick skipped, another instance holds the lock")
		telemetry.WorkerTicksTotal.WithLabelValues(s.name, "skipped").Inc()
	case workErr != nil:
		s.log.ErrorContext(ctx, msgPassFailed, "error", workErr)
		s.recordHealth(false, workErr)
	default:
		s.recordHealth(true, nil)
	}
}

// recordHealth updates the per-service health state plus the worker
// metrics. Centralized so the tick path has one place to maintain the
// invariants (consecutiveFailures resets on success, last_success
// timestamp only moves forward on success).
func (s *lockedTickerService) recordHealth(success bool, err error) {
	now := time.Now()
	s.healthMu.Lock()
	if success {
		s.health.LastSuccess = now
		s.health.LastError = ""
		s.health.ConsecutiveFailures = 0
	} else {
		s.health.LastFailure = now
		if err != nil {
			s.health.LastError = err.Error()
		}
		s.health.ConsecutiveFailures++
	}
	failures := s.health.ConsecutiveFailures
	s.healthMu.Unlock()

	if success {
		telemetry.WorkerTicksTotal.WithLabelValues(s.name, "success").Inc()
		telemetry.WorkerLastSuccessTimestampSeconds.WithLabelValues(s.name).Set(float64(now.Unix()))
		telemetry.WorkerConsecutiveFailures.WithLabelValues(s.name).Set(0)
	} else {
		telemetry.WorkerTicksTotal.WithLabelValues(s.name, "error").Inc()
		telemetry.WorkerConsecutiveFailures.WithLabelValues(s.name).Set(float64(failures))
	}
}

// Health implements lifecycle.HealthReporter. Returns a snapshot of
// the service's last tick outcomes plus its registered name so the
// admin endpoint can render a per-service status table.
func (s *lockedTickerService) Health() lifecycle.WorkerHealth {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	h := s.health
	h.Name = s.name
	return h
}

// -------------------------------------------------------------------------
// USAGE FLUSH (unique: no advisory lock, adaptive interval)
// -------------------------------------------------------------------------

// usageFlushService periodically flushes in-memory usage counters to the
// database, acquiring an advisory lock only when Redis counters are active.
type usageFlushService struct {
	manager *proxy.BackendManager
	locker  advisoryLocker
	log     *slog.Logger
}

// NewUsageFlushService constructs the usage flush background service.
func NewUsageFlushService(manager *proxy.BackendManager, locker advisoryLocker) lifecycle.Runner {
	return &usageFlushService{
		manager: manager,
		locker:  locker,
		log:     componentLogger("usage_flush"),
	}
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
					s.log.InfoContext(ctx, "interval adjusted", "interval", targetInterval)
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
		acquired, err := s.locker.WithAdvisoryLock(ctx, core.LockUsageFlush,
			func(lockCtx context.Context) error {
				s.doFlush(lockCtx)
				return nil
			})
		if err != nil && !errors.Is(err, core.ErrDBUnavailable) {
			s.log.ErrorContext(ctx, "tick failed", "error", err)
		}
		if !acquired {
			s.log.DebugContext(ctx, "tick skipped, another instance holds the lock")
		}
		return
	}
	s.doFlush(ctx)
}

// doFlush performs the actual flush and quota metric update.
func (s *usageFlushService) doFlush(ctx context.Context) {
	if err := s.manager.FlushUsage(ctx); err != nil && !errors.Is(err, core.ErrDBUnavailable) {
		s.log.ErrorContext(ctx, "counter flush failed", "error", err)
	}
	if err := s.manager.UpdateQuotaMetrics(ctx); err != nil && !errors.Is(err, core.ErrDBUnavailable) {
		s.log.ErrorContext(ctx, "quota metrics refresh failed", "error", err)
	}
}

// -------------------------------------------------------------------------
// SERVICE CONSTRUCTORS
// -------------------------------------------------------------------------

// NewMultipartCleanupService constructs the multipart-cleanup background service.
func NewMultipartCleanupService(manager *proxy.BackendManager, locker advisoryLocker, staleTimeout time.Duration) lifecycle.Runner {
	if staleTimeout <= 0 {
		staleTimeout = defaultMultipartStaleTimeout
	}
	const slug = "multipart_cleanup"
	return &lockedTickerService{
		locker:   locker,
		interval: defaultMultipartCleanupTick,
		lockID:   core.LockMultipartCleanup,
		name:     slug,
		log:      componentLogger(slug),
		work: func(ctx context.Context) error {
			manager.MultipartManager.CleanupStaleMultipartUploads(ctx, staleTimeout)
			return nil
		},
	}
}

// NewCleanupQueueService constructs the cleanup-queue background service.
func NewCleanupQueueService(cleanup *worker.CleanupWorker, locker advisoryLocker) lifecycle.Runner {
	const slug = "cleanup_queue"
	log := componentLogger(slug)
	return &lockedTickerService{
		locker:   locker,
		interval: defaultCleanupQueueTick,
		lockID:   core.LockCleanupQueue,
		name:     slug,
		log:      log,
		work: func(ctx context.Context) error {
			processed, failed := cleanup.ProcessCleanupQueue(ctx)
			if processed > 0 || failed > 0 {
				log.InfoContext(ctx, "queue processed", "processed", processed, "failed", failed)
			}
			return nil
		},
	}
}

// NewPendingReaperService constructs the pending-objects reaper background
// service. The reaper resolves abandoned PUT intents by HEADing the
// destination backend and either promoting the intent into object_locations
// (bytes present) or dropping it (bytes absent). Returns nil when no
// pending reaper is configured.
func NewPendingReaperService(reaper *worker.PendingReaper, locker advisoryLocker, tick time.Duration) lifecycle.Runner {
	if reaper == nil {
		return nil
	}
	if tick <= 0 {
		tick = defaultPendingReaperTick
	}
	const slug = "pending_reaper"
	log := componentLogger(slug)
	return &lockedTickerService{
		locker:   locker,
		interval: tick,
		lockID:   core.LockPendingReaper,
		name:     slug,
		log:      log,
		work: func(ctx context.Context) error {
			resolved, failed := reaper.ProcessPendingQueue(ctx)
			if resolved > 0 || failed > 0 {
				log.InfoContext(ctx, "pending queue processed", "resolved", resolved, "failed", failed)
			}
			return nil
		},
	}
}

// NewRebalancerService constructs the rebalancer background service.
func NewRebalancerService(manager *proxy.BackendManager, rebalancer *worker.Rebalancer, locker advisoryLocker) lifecycle.Runner {
	interval := defaultRebalanceInterval
	if rcfg := rebalancer.Config(); rcfg != nil && rcfg.Interval > 0 {
		interval = rcfg.Interval
	}
	const slug = "rebalance"
	log := componentLogger(slug)
	return &lockedTickerService{
		locker:   locker,
		interval: interval,
		lockID:   core.LockRebalancer,
		name:     slug,
		log:      log,
		shouldRun: func() bool {
			rcfg := rebalancer.Config()
			return rcfg != nil && rcfg.Enabled
		},
		work: func(ctx context.Context) error {
			rcfg := rebalancer.Config()
			if rcfg == nil {
				return nil
			}
			moved, err := rebalancer.Rebalance(ctx, *rcfg)
			return handlePassResult(ctx, log, manager, moved, err, "objects_moved")
		},
	}
}

// NewLifecycleService constructs the lifecycle-expiration background service.
func NewLifecycleService(manager *proxy.BackendManager, locker advisoryLocker) lifecycle.Runner {
	const slug = "lifecycle"
	log := componentLogger(slug)
	return &lockedTickerService{
		locker:   locker,
		interval: defaultLifecycleTick,
		lockID:   core.LockLifecycle,
		name:     slug,
		log:      log,
		shouldRun: func() bool {
			cfg := manager.LifecycleConfig()
			return cfg != nil && len(cfg.Rules) > 0
		},
		work: func(ctx context.Context) error {
			cfg := manager.LifecycleConfig()
			if cfg == nil {
				return nil
			}
			deleted, failed := manager.ProcessLifecycleRules(ctx, cfg.Rules)
			if deleted > 0 || failed > 0 {
				log.InfoContext(ctx, "expiration completed",
					"deleted", deleted, "failed", failed)
			}
			if failed > 0 {
				telemetry.LifecycleRunsTotal.WithLabelValues("partial").Inc()
			} else {
				telemetry.LifecycleRunsTotal.WithLabelValues("success").Inc()
			}
			return nil
		},
		onError: func(err error) {
			log.ErrorContext(context.Background(), "expiration failed", "error", err)
			telemetry.LifecycleRunsTotal.WithLabelValues("error").Inc()
		},
	}
}

// NewOverReplicationService constructs the over-replication cleanup service.
func NewOverReplicationService(manager *proxy.BackendManager, overRep *worker.OverReplicationCleaner, locker advisoryLocker) lifecycle.Runner {
	interval := defaultOverReplicationTick
	if rcfg := overRep.Config(); rcfg != nil && rcfg.WorkerInterval > 0 {
		interval = rcfg.WorkerInterval
	}
	const slug = "over_replication_cleanup"
	log := componentLogger(slug)
	return &lockedTickerService{
		locker:   locker,
		interval: interval,
		lockID:   core.LockOverReplication,
		name:     slug,
		log:      log,
		shouldRun: func() bool {
			rcfg := overRep.Config()
			return rcfg != nil && rcfg.Factor > 1
		},
		work: func(ctx context.Context) error {
			rcfg := overRep.Config()
			if rcfg == nil {
				return nil
			}
			removed, err := overRep.Clean(ctx, *rcfg)
			return handlePassResult(ctx, log, manager, removed, err, "copies_removed")
		},
	}
}

// NewReplicatorService constructs the replication background service.
func NewReplicatorService(manager *proxy.BackendManager, replicator *worker.Replicator, locker advisoryLocker) lifecycle.Runner {
	const slug = "replication"
	log := componentLogger(slug)
	replicateWork := func(ctx context.Context) error {
		rcfg := replicator.Config()
		if rcfg == nil {
			return nil
		}
		created, err := replicator.Replicate(ctx, *rcfg)
		return handlePassResult(ctx, log, manager, created, err, "copies_created")
	}

	interval := defaultReplicatorTick
	if rcfg := replicator.Config(); rcfg != nil && rcfg.WorkerInterval > 0 {
		interval = rcfg.WorkerInterval
	}
	return &lockedTickerService{
		locker:   locker,
		interval: interval,
		lockID:   core.LockReplicator,
		name:     slug,
		log:      log,
		shouldRun: func() bool {
			rcfg := replicator.Config()
			return rcfg != nil && rcfg.Factor > 1
		},
		startup: replicateWork,
		work:    replicateWork,
	}
}

// NewReconcileService constructs the reconcile background service.
func NewReconcileService(reconciler *worker.Reconciler, locker advisoryLocker, interval time.Duration) lifecycle.Runner {
	const slug = "reconcile"
	return &lockedTickerService{
		locker:   locker,
		interval: interval,
		lockID:   core.LockReconcile,
		name:     slug,
		log:      componentLogger(slug),
		work: func(ctx context.Context) error {
			reconciler.Run(ctx)
			return nil
		},
	}
}

// -------------------------------------------------------------------------
// CIRCUIT BREAKER WATCHDOG
// -------------------------------------------------------------------------

// circuitBreakerWatchdog periodically resets stale half-open probes on every
// breaker registered in the breaker.Registry. This prevents circuits from
// getting stuck half-open indefinitely when no new requests arrive. Membership
// in the registry is decided once at DI construction time, so the watchdog
// itself contains no type-assertion or backend-discovery logic.
type circuitBreakerWatchdog struct {
	registry *breaker.Registry
}

// NewCircuitBreakerWatchdog constructs the watchdog background service.
func NewCircuitBreakerWatchdog(registry *breaker.Registry) lifecycle.Runner {
	return &circuitBreakerWatchdog{registry: registry}
}

// Run implements lifecycle.Runner. Checks every defaultCircuitBreakerWatchdog
// (1 minute)  -  half the breaker probe timeout.
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

// checkAll resets stale probes on every registered breaker.
func (w *circuitBreakerWatchdog) checkAll() {
	w.registry.ResetStaleProbes()
}

// -------------------------------------------------------------------------
// SCRUBBER
// -------------------------------------------------------------------------

// NewScrubberService constructs the integrity scrubber background service.
func NewScrubberService(scrubber *worker.Scrubber, locker advisoryLocker) lifecycle.Runner {
	interval := defaultScrubberInterval
	if icfg := scrubber.Config(); icfg != nil && icfg.ScrubberInterval > 0 {
		interval = icfg.ScrubberInterval
	}
	const slug = "scrubber"
	log := componentLogger(slug)
	return &lockedTickerService{
		locker:   locker,
		interval: interval,
		lockID:   core.LockScrubber,
		name:     slug,
		log:      log,
		shouldRun: func() bool {
			icfg := scrubber.Config()
			return icfg != nil && icfg.Enabled && icfg.ScrubberInterval > 0
		},
		work: func(ctx context.Context) error {
			icfg := scrubber.Config()
			if icfg == nil {
				return nil
			}
			checked, failed := scrubber.Scrub(ctx, icfg.ScrubberBatchSize)
			if checked > 0 || failed > 0 {
				log.InfoContext(ctx, "scrub completed", "checked", checked, "failed", failed)
			}
			return nil
		},
	}
}
