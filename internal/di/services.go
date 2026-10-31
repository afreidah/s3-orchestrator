// -------------------------------------------------------------------------------
// Background Service Definitions - Manager-Coupled
//
// Author: Alex Freidah
//
// Background services whose run-loop semantics live in DI because they
// read state from several collaborators at once, through the consumer
// interfaces declared in service_interfaces.go:
//
//   - usageFlushService: adapts its tick interval at runtime based on
//     observed load; does not fit the plain tickrunner.Service shape.
//   - fleetSnapshotService: recomputes the fleet-wide gauges on the
//     telemetry.metrics.fleet_interval cadence through the runtime.
//   - lifecycleService: a small tickrunner wrapper that needs the
//     manager-side lifecycleOps surface to read rules and process them.
//   - topicWatcher: applies what another instance announces over Redis,
//     a provisioning change or a cache invalidation, to this instance.
//
// All other background-service factories live next to their owning
// worker (internal/worker, internal/proxy/multipart, internal/breaker)
// so the lifecycle.Runner constructor sits next to the work it owns.
// -------------------------------------------------------------------------------

package di

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle/tickrunner"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/sharedstate"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
)

// defaultUsageFlushInterval is the usage-flush service's tick cadence
// when the config does not specify one. The lifecycleService uses
// defaultLifecycleTick.
const (
	defaultUsageFlushInterval = 30 * time.Second
	defaultLifecycleTick      = 1 * time.Hour
)

// -------------------------------------------------------------------------
// USAGE FLUSH
// -------------------------------------------------------------------------

// usageFlushService periodically flushes usage counters to the database on
// every instance. No lock is needed: each instance flushes its own local
// counters, and the store applies a staged shared batch at most once whichever
// instance finds it.
type usageFlushService struct {
	UsageFlushDeps
	log *slog.Logger
}

// UsageFlushDeps groups what the flush tick draws on: the usage service that
// owns the flush, the counters that say whether to tick faster, the runtime
// that republishes the gauges afterwards, and the backend state manager whose
// cached states the tick reloads.
type UsageFlushDeps struct {
	Flusher usageFlushOps
	Tracker nearLimitReporter
	Fleet   quotaMetricsRefresher
	States  backendStateRefresher
}

// NewUsageFlushService constructs the usage flush background service.
func NewUsageFlushService(d *UsageFlushDeps) lifecycle.Runner {
	must.NotNil("d", d)
	must.NotNil("d.Flusher", d.Flusher)
	must.NotNil("d.Tracker", d.Tracker)
	must.NotNil("d.Fleet", d.Fleet)
	must.NotNil("d.States", d.States)
	return &usageFlushService{UsageFlushDeps: *d, log: tickrunner.ComponentLogger("usage_flush")}
}

// Run periodically flushes in-memory usage counters and adapts the tick
// interval toward FastInterval when a backend nears its limits.
func (s *usageFlushService) Run(ctx context.Context) error {
	cfg := s.Flusher.Config()
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
			currentInterval = s.adjustInterval(ctx, ticker, currentInterval)
		case <-ctx.Done():
			return nil
		}
	}
}

// adjustInterval reconfigures the flush ticker when the reloaded config or the
// adaptive fast-path changes the target interval, and returns the interval now
// in effect (unchanged when nothing moved).
func (s *usageFlushService) adjustInterval(ctx context.Context, ticker *time.Ticker, current time.Duration) time.Duration {
	cfg := s.Flusher.Config()
	if cfg == nil {
		return current
	}
	target := cfg.Interval
	if cfg.AdaptiveEnabled && s.Tracker.NearLimit(cfg.AdaptiveThreshold) {
		target = cfg.FastInterval
	}
	if target == current {
		return current
	}
	ticker.Reset(target)
	s.log.InfoContext(ctx, "interval adjusted", "interval", target)
	return target
}

// flushTick runs a single flush+metrics cycle on every instance. With Redis
// counters every instance also loads the fleet snapshot and worker gauges last
// published. Every instance then reloads its own usage baselines, or its limit
// checks would keep admitting work against budget already spent. The fleet
// snapshot itself is computed by the fleet snapshot service on its own
// cadence.
func (s *usageFlushService) flushTick(ctx context.Context) {
	s.logUnlessDBDown(ctx, "quota baseline refresh failed", s.Flusher.RefreshQuotaBaselines(ctx))
	s.logUnlessDBDown(ctx, "counter flush failed", s.Flusher.FlushUsage(ctx))
	if s.Flusher.RedisCounterConfigured() {
		s.loadFleetState(ctx)
	}

	// After the flush, so the baseline includes what it wrote.
	s.logUnlessDBDown(ctx, "usage baseline refresh failed", s.Fleet.RefreshUsageBaselines(ctx))

	// This service runs on every instance in every mode, while the drain
	// worker that also refreshes the cache runs on one instance at a time.
	// A stale cache only costs ranking: admission reads the records itself.
	s.logUnlessDBDown(ctx, "backend state refresh failed", s.States.Refresh(ctx))
}

// logUnlessDBDown logs a failed tick step. A database outage is skipped: the
// breaker already reports it, and every step would repeat it each tick.
func (s *usageFlushService) logUnlessDBDown(ctx context.Context, msg string, err error) {
	if err != nil && !errors.Is(err, core.ErrDBUnavailable) {
		s.log.ErrorContext(ctx, msg, "error", err)
	}
}

// loadFleetState loads the fleet snapshot and the worker gauges, which their
// own lock holders published and which need not be this instance.
func (s *usageFlushService) loadFleetState(ctx context.Context) {
	if err := s.Fleet.LoadFleetMetrics(ctx); err != nil {
		s.log.WarnContext(ctx, "fleet snapshot load failed", "error", err)
	}
	if err := s.Fleet.LoadWorkerGauges(ctx); err != nil {
		s.log.WarnContext(ctx, "worker gauges load failed", "error", err)
	}
}

// -------------------------------------------------------------------------
// FLEET SNAPSHOT
// -------------------------------------------------------------------------

// NewFleetSnapshotService constructs the service that recomputes the
// fleet-wide gauges and admin status figures every interval. It runs in every
// mode under its own advisory lock; an instance that finds a fresh snapshot
// already published applies it instead of scanning the ledger again.
func NewFleetSnapshotService(fleet fleetSnapshotRefresher, locker tickrunner.AdvisoryLocker, interval time.Duration) lifecycle.Runner {
	const slug = "fleet_snapshot"
	return tickrunner.New(tickrunner.Config{
		Locker:   locker,
		Interval: interval,
		LockID:   core.LockFleetSnapshot,
		Name:     slug,
		Log:      tickrunner.ComponentLogger(slug),
		Work: func(ctx context.Context) error {
			if err := fleet.RefreshFleetIfStale(ctx); err != nil && !errors.Is(err, core.ErrDBUnavailable) {
				return err
			}
			return nil
		},
	})
}

// -------------------------------------------------------------------------
// LIFECYCLE
// -------------------------------------------------------------------------

// NewLifecycleService constructs the lifecycle-expiration background
// service. Lives in DI (rather than next to a worker) because the work
// surface is on *expiry.Manager via the lifecycleOps consumer interface -
// there is no dedicated worker type.
func NewLifecycleService(manager lifecycleOps, locker tickrunner.AdvisoryLocker) lifecycle.Runner {
	const slug = "lifecycle"
	log := tickrunner.ComponentLogger(slug)
	return tickrunner.New(tickrunner.Config{
		Locker:   locker,
		Interval: defaultLifecycleTick,
		LockID:   core.LockLifecycle,
		Name:     slug,
		Log:      log,
		ShouldRun: func() bool {
			cfg := manager.Config()
			return cfg != nil && len(cfg.Rules) > 0
		},
		Work: func(ctx context.Context) error {
			cfg := manager.Config()
			if cfg == nil {
				return nil
			}
			// No observer: nothing is watching a scheduled tick.
			sum := manager.ProcessRules(ctx, cfg.Rules, nil)
			if sum.Succeeded > 0 || sum.Failed > 0 {
				log.InfoContext(ctx, "expiration completed",
					"deleted", sum.Succeeded, "failed", sum.Failed)
				event.Publish(event.LifecycleCompleted, "", map[string]any{
					"deleted": sum.Succeeded,
					"failed":  sum.Failed,
				})
			}
			if sum.Failed > 0 {
				telemetry.LifecycleRunsTotal.WithLabelValues("partial").Inc()
			} else {
				telemetry.LifecycleRunsTotal.WithLabelValues("success").Inc()
			}
			return nil
		},
		OnError: func(err error) {
			log.ErrorContext(context.Background(), "expiration failed", "error", err)
			telemetry.LifecycleRunsTotal.WithLabelValues("error").Inc()
		},
	})
}

// -------------------------------------------------------------------------
// SHARED-STATE WATCHERS
// -------------------------------------------------------------------------

// topicWatcher applies what other instances announce on one topic, and
// resyncs whenever this instance may have missed an announcement: as its
// subscription is established, and for a message it cannot read.
type topicWatcher[T any] struct {
	topic  sharedstate.Topic[T]
	apply  func(context.Context, T)
	resync func(context.Context)
}

// newTopicWatcher constructs the watch service for one topic.
func newTopicWatcher[T any](topic sharedstate.Topic[T], apply func(context.Context, T), resync func(context.Context)) lifecycle.Runner {
	must.NotNil("apply", apply)
	must.NotNil("resync", resync)
	return &topicWatcher[T]{topic: topic, apply: apply, resync: resync}
}

// Run watches the topic until ctx is done.
func (w *topicWatcher[T]) Run(ctx context.Context) error {
	w.topic.Watch(ctx, w.apply, w.resync)
	return nil
}

// newProvisioningWatcher rebuilds this instance's provisioning view whenever
// another instance announces a change, and on every resync. apply rebuilds
// without announcing, since the change came from another instance.
func newProvisioningWatcher(shared *sharedstate.Shared, apply func(ctx context.Context) error) lifecycle.Runner {
	must.NotNil("apply", apply)
	log := tickrunner.ComponentLogger("provisioning_watch")
	rebuild := func(ctx context.Context) {
		if err := apply(ctx); err != nil {
			log.ErrorContext(ctx, "provisioning rebuild failed", logfmt.Err(err))
		}
	}
	return newTopicWatcher(provisioningTopic(shared), func(ctx context.Context, _ struct{}) { rebuild(ctx) }, rebuild)
}

// newCacheWatcher applies the cache flushes and invalidations other instances
// announce to this instance's cache, and empties it on every resync, since an
// invalidation it missed would otherwise leave it serving dropped entries.
func newCacheWatcher(shared *sharedstate.Shared, c cache.ObjectCache) lifecycle.Runner {
	must.NotNil("c", c)
	return newTopicWatcher(cache.InvalidationTopic(shared),
		func(_ context.Context, inv cache.Invalidation) { inv.Apply(c) },
		func(context.Context) { c.Clear() })
}
