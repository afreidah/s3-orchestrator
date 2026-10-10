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
//   - provisioningWatcher: rebuilds the provisioning view when another
//     instance announces a change over Redis.
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

	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle/tickrunner"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
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

// usageFlushService periodically flushes in-memory usage counters to the
// database, acquiring an advisory lock only when Redis counters are active.
type usageFlushService struct {
	flusher usageFlushOps
	tracker nearLimitReporter
	fleet   quotaMetricsRefresher
	drains  drainStateRefresher
	locker  tickrunner.AdvisoryLocker
	log     *slog.Logger
}

// UsageFlushDeps groups what the flush tick draws on: the usage service that
// owns the flush, the counters that say whether to tick faster, the runtime
// that republishes the gauges afterwards, and the drain manager whose cached
// drain states the tick reloads.
type UsageFlushDeps struct {
	Flusher usageFlushOps
	Tracker nearLimitReporter
	Fleet   quotaMetricsRefresher
	Drains  drainStateRefresher
	Locker  tickrunner.AdvisoryLocker
}

// NewUsageFlushService constructs the usage flush background service.
func NewUsageFlushService(d *UsageFlushDeps) lifecycle.Runner {
	must.NotNil("d", d)
	must.NotNil("d.Flusher", d.Flusher)
	must.NotNil("d.Tracker", d.Tracker)
	must.NotNil("d.Fleet", d.Fleet)
	must.NotNil("d.Drains", d.Drains)
	return &usageFlushService{
		flusher: d.Flusher,
		tracker: d.Tracker,
		fleet:   d.Fleet,
		drains:  d.Drains,
		locker:  d.Locker,
		log:     tickrunner.ComponentLogger("usage_flush"),
	}
}

// Run periodically flushes in-memory usage counters and adapts the tick
// interval toward FastInterval when a backend nears its limits.
func (s *usageFlushService) Run(ctx context.Context) error {
	cfg := s.flusher.Config()
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
	cfg := s.flusher.Config()
	if cfg == nil {
		return current
	}
	target := cfg.Interval
	if cfg.AdaptiveEnabled && s.tracker.NearLimit(cfg.AdaptiveThreshold) {
		target = cfg.FastInterval
	}
	if target == current {
		return current
	}
	ticker.Reset(target)
	s.log.InfoContext(ctx, "interval adjusted", "interval", target)
	return target
}

// flushTick runs a single flush+metrics cycle. With Redis counters only the
// advisory lock holder performs the destructive GETSET, and every instance
// loads the fleet snapshot and worker gauges last published. Every instance
// then reloads its own usage baselines, or its limit checks would keep
// admitting work against budget already spent. The fleet snapshot itself is
// computed by the fleet snapshot service on its own cadence.
func (s *usageFlushService) flushTick(ctx context.Context) {
	// Outside the advisory lock: the byte deltas are this instance's own, so
	// every instance flushes its own set. Skipping them on a lost lock would
	// leave bytes_used short of what this instance wrote.
	if err := s.flusher.FlushQuota(ctx); err != nil && !errors.Is(err, core.ErrDBUnavailable) {
		s.log.ErrorContext(ctx, "quota flush failed", "error", err)
	}

	if s.flusher.RedisCounterConfigured() {
		s.flushFleetTick(ctx)
	} else {
		s.flushUsage(ctx)
	}

	// After the flush, so the lock holder's baseline includes what it wrote.
	if err := s.fleet.RefreshUsageBaselines(ctx); err != nil && !errors.Is(err, core.ErrDBUnavailable) {
		s.log.ErrorContext(ctx, "usage baseline refresh failed", "error", err)
	}

	// This service runs on every instance in every mode, while the drain
	// worker that also refreshes the cache runs on one instance at a time.
	// A stale cache only costs ranking: admission reads the records itself.
	if err := s.drains.Refresh(ctx); err != nil && !errors.Is(err, core.ErrDBUnavailable) {
		s.log.ErrorContext(ctx, "drain state refresh failed", "error", err)
	}
}

// flushFleetTick is the shared-counter half of a tick: the lock holder flushes
// the counters, and every instance loads the fleet snapshot and the worker
// gauges, which their own lock holders published and which need not be this
// instance or the flush lock's holder.
func (s *usageFlushService) flushFleetTick(ctx context.Context) {
	acquired, err := s.locker.WithAdvisoryLock(ctx, core.LockUsageFlush,
		func(lockCtx context.Context) error {
			s.flushUsage(lockCtx)
			return nil
		})
	if err != nil && !errors.Is(err, core.ErrDBUnavailable) {
		s.log.ErrorContext(ctx, "tick failed", "error", err)
	}
	if !acquired {
		s.log.DebugContext(ctx, "usage flush skipped, another instance holds the lock")
	}
	if err := s.fleet.LoadFleetMetrics(ctx); err != nil {
		s.log.WarnContext(ctx, "fleet snapshot load failed", "error", err)
	}
	if err := s.fleet.LoadWorkerGauges(ctx); err != nil {
		s.log.WarnContext(ctx, "worker gauges load failed", "error", err)
	}
}

// flushUsage writes the usage counters to the store. With Redis counters it
// runs only under the advisory lock.
func (s *usageFlushService) flushUsage(ctx context.Context) {
	if err := s.flusher.FlushUsage(ctx); err != nil && !errors.Is(err, core.ErrDBUnavailable) {
		s.log.ErrorContext(ctx, "counter flush failed", "error", err)
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
// PROVISIONING WATCH
// -------------------------------------------------------------------------

// provisioningWatcher rebuilds this instance's provisioning view when another
// instance announces a change, and each time its subscription is established,
// since it may have missed an announcement while it was not subscribed.
type provisioningWatcher struct {
	channel sharedChannelWatcher
	apply   func(ctx context.Context) error
	log     *slog.Logger
}

// newProvisioningWatcher constructs the provisioning watch service. apply
// rebuilds this instance's view without announcing, so rebuilds do not echo
// between instances.
func newProvisioningWatcher(channel sharedChannelWatcher, apply func(ctx context.Context) error) lifecycle.Runner {
	must.NotNil("channel", channel)
	must.NotNil("apply", apply)
	return &provisioningWatcher{
		channel: channel,
		apply:   apply,
		log:     tickrunner.ComponentLogger("provisioning_watch"),
	}
}

// Run watches the provisioning channel until ctx is done.
func (w *provisioningWatcher) Run(ctx context.Context) error {
	w.channel.WatchShared(ctx, provisioningChannel, func(ctx context.Context) {
		if err := w.apply(ctx); err != nil {
			w.log.ErrorContext(ctx, "provisioning rebuild failed", "error", err)
		}
	})
	return nil
}
