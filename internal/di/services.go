// -------------------------------------------------------------------------------
// Background Service Definitions - Manager-Coupled
//
// Author: Alex Freidah
//
// Background services whose run-loop semantics live in DI because they
// read state from several collaborators at once, through the consumer
// interfaces declared in service_interfaces.go:
//
//   - usageFlushService: runs on every instance with no lock, and adapts
//     its tick interval at runtime based on observed load.
//   - fleetSnapshotService: recomputes the fleet-wide gauges on the
//     telemetry.metrics.fleet_interval cadence through the metrics collector.
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
	"fmt"
	"log/slog"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle/tickrunner"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/sharedstate"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// lifecycleTick is the lifecycle service's tick cadence.
const lifecycleTick = 1 * time.Hour

// -------------------------------------------------------------------------
// USAGE FLUSH
// -------------------------------------------------------------------------

// usageFlushService periodically flushes usage counters to the database on
// every instance. No lock is needed: each instance flushes its own local
// counters, and the store applies a staged shared batch at most once whichever
// instance finds it.
type usageFlushService struct {
	UsageFlushDeps
	log     *slog.Logger
	current time.Duration
}

// UsageFlushDeps groups what the flush tick draws on: the usage service that
// owns the flush, the counters that say whether to tick faster, the metrics
// collector that republishes the gauges afterwards, the backend state manager whose
// cached states the tick reloads, and the usage-flush section of the running
// configuration.
type UsageFlushDeps struct {
	Flusher usageFlushOps
	Tracker nearLimitReporter
	Fleet   quotaMetricsRefresher
	States  backendStateRefresher
	Config  syncutil.Section[config.UsageFlushConfig]
}

// NewUsageFlushService constructs the usage flush background service, which
// runs on every instance with no lock. Its interval adapts toward
// FastInterval when a backend nears its limits.
func NewUsageFlushService(d *UsageFlushDeps) lifecycle.Runner {
	s := newUsageFlush(d)
	return tickrunner.New(tickrunner.Config{
		Name:     usageFlushSlug,
		Interval: s.current,
		Next:     s.nextInterval,
		Work:     s.flushTick,
	})
}

// usageFlushSlug names the usage flush service and its log component.
const usageFlushSlug = "usage_flush"

// newUsageFlush builds the flush the service ticks, starting on the
// configured interval.
func newUsageFlush(d *UsageFlushDeps) *usageFlushService {
	must.NotNil("d", d)
	must.NotNil("d.Flusher", d.Flusher)
	must.NotNil("d.Tracker", d.Tracker)
	must.NotNil("d.Fleet", d.Fleet)
	must.NotNil("d.States", d.States)
	must.NotNil("d.Config", d.Config)
	return &usageFlushService{
		UsageFlushDeps: *d,
		log:            logfmt.Logger(usageFlushSlug),
		current:        d.Config.Config().Interval,
	}
}

// nextInterval returns the wait before the next flush: the configured
// interval, or the fast one while a backend nears its limits. A change is
// logged once, when it happens.
func (s *usageFlushService) nextInterval() time.Duration {
	cfg := s.Config.Config()
	target := cfg.Interval
	if cfg.AdaptiveEnabled && s.Tracker.NearLimit(cfg.AdaptiveThreshold) {
		target = cfg.FastInterval
	}
	if target != s.current {
		s.current = target
		s.log.InfoContext(context.Background(), "interval adjusted", "interval", target)
	}
	return target
}

// flushTick runs a single flush+metrics cycle on every instance. With Redis
// counters every instance also loads the fleet snapshot and worker gauges last
// published. Every instance then reloads its own usage baselines, or its limit
// checks would keep admitting work against budget already spent. The fleet
// snapshot itself is computed by the fleet snapshot service on its own
// cadence. Every step runs whatever the others did, and their failures are
// returned together.
func (s *usageFlushService) flushTick(ctx context.Context) error {
	quotaErr := wrapStep("refresh quota baselines", s.Flusher.RefreshQuotaBaselines(ctx))
	flushErr := wrapStep("flush counters", s.Flusher.FlushUsage(ctx))
	if s.Flusher.RedisCounterConfigured() {
		s.loadFleetState(ctx)
	}

	// After the flush, so the baseline includes what it wrote.
	usageErr := wrapStep("refresh usage baselines", s.Fleet.RefreshUsageBaselines(ctx))

	// This service runs on every instance in every mode, while the drain
	// worker that also refreshes the cache runs on one instance at a time.
	// A stale cache only costs ranking: admission reads the records itself.
	statesErr := wrapStep("refresh backend states", s.States.Refresh(ctx))
	return errors.Join(quotaErr, flushErr, usageErr, statesErr)
}

// wrapStep names a failed tick step in its error, and passes nil through.
func wrapStep(step string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", step, err)
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
func NewFleetSnapshotService(fleet fleetSnapshotRefresher, locker core.AdvisoryLocker, interval time.Duration) lifecycle.Runner {
	return tickrunner.New(tickrunner.Config{
		Locker:   locker,
		Interval: interval,
		LockID:   core.LockFleetSnapshot,
		Name:     "fleet_snapshot",
		Work:     fleet.RefreshFleetIfStale,
	})
}

// -------------------------------------------------------------------------
// LIFECYCLE
// -------------------------------------------------------------------------

// NewLifecycleService constructs the lifecycle-expiration background
// service. Lives in DI (rather than next to a worker) because the work
// surface is on *expiry.Manager via the lifecycleOps consumer interface -
// there is no dedicated worker type.
func NewLifecycleService(manager lifecycleOps, locker core.AdvisoryLocker) lifecycle.Runner {
	return tickrunner.New(tickrunner.Config{
		Locker:   locker,
		Interval: lifecycleTick,
		LockID:   core.LockLifecycle,
		Name:     string(audit.WorkloadLifecycle),
		ShouldRun: func() bool {
			return len(manager.Config().Rules) > 0
		},
		Work: func(ctx context.Context) error {
			// No observer: nothing is watching a scheduled tick.
			manager.ProcessRules(ctx, manager.Config().Rules, nil)
			return nil
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
	name   string
	topic  sharedstate.Topic[T]
	apply  func(context.Context, T)
	resync func(context.Context)
}

// newTopicWatcher constructs the watch service for one topic, registered and
// logged under name.
func newTopicWatcher[T any](name string, topic sharedstate.Topic[T], apply func(context.Context, T), resync func(context.Context)) lifecycle.Runner {
	must.NotNil("apply", apply)
	must.NotNil("resync", resync)
	return &topicWatcher[T]{name: name, topic: topic, apply: apply, resync: resync}
}

// Run watches the topic until ctx is done.
func (w *topicWatcher[T]) Run(ctx context.Context) error {
	w.topic.Watch(ctx, w.apply, w.resync)
	return nil
}

// Name returns the name the watcher was built with, which the lifecycle
// manager registers it under.
func (w *topicWatcher[T]) Name() string { return w.name }

// Watcher names, which are also their log components.
const (
	provisioningWatchName = "provisioning_watch"
	cacheWatchName        = "cache_watch"
)

// newProvisioningWatcher rebuilds this instance's provisioning view whenever
// another instance announces a change, and on every resync. apply rebuilds
// without announcing, since the change came from another instance.
func newProvisioningWatcher(shared *sharedstate.Shared, apply func(ctx context.Context) error) lifecycle.Runner {
	must.NotNil("apply", apply)
	log := logfmt.Logger(provisioningWatchName)
	rebuild := func(ctx context.Context) {
		if err := apply(ctx); err != nil {
			log.ErrorContext(ctx, "provisioning rebuild failed", "error", err)
		}
	}
	return newTopicWatcher(provisioningWatchName, provisioningTopic(shared), func(ctx context.Context, _ struct{}) { rebuild(ctx) }, rebuild)
}

// newCacheWatcher applies the cache flushes and invalidations other instances
// announce to this instance's cache, and empties it on every resync, since an
// invalidation it missed would otherwise leave it serving dropped entries.
func newCacheWatcher(shared *sharedstate.Shared, c cache.ObjectCache) lifecycle.Runner {
	must.NotNil("c", c)
	return newTopicWatcher(cacheWatchName, cache.InvalidationTopic(shared),
		func(_ context.Context, inv cache.Invalidation) { inv.Apply(c) },
		func(context.Context) { c.Clear() })
}
