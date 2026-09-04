// -------------------------------------------------------------------------------
// Background Service Definitions - Manager-Coupled
//
// Author: Alex Freidah
//
// Two background services whose run-loop semantics live in DI because they
// read state from several collaborators at once, through the consumer
// interfaces declared in service_interfaces.go:
//
//   - usageFlushService: adapts its tick interval at runtime based on
//     observed load; does not fit the plain tickrunner.Service shape.
//   - lifecycleService: a small tickrunner wrapper that needs the
//     manager-side lifecycleOps surface to read rules and process them.
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
// USAGE FLUSH (unique: no advisory lock, adaptive interval)
// -------------------------------------------------------------------------

// usageFlushService periodically flushes in-memory usage counters to the
// database, acquiring an advisory lock only when Redis counters are active.
type usageFlushService struct {
	flusher usageFlushOps
	tracker nearLimitReporter
	fleet   quotaMetricsRefresher
	locker  tickrunner.AdvisoryLocker
	log     *slog.Logger
}

// UsageFlushDeps groups what the flush tick draws on: the usage service that
// owns the flush, the counters that say whether to tick faster, and the
// runtime that republishes the gauges afterwards.
type UsageFlushDeps struct {
	Flusher usageFlushOps
	Tracker nearLimitReporter
	Fleet   quotaMetricsRefresher
	Locker  tickrunner.AdvisoryLocker
}

// NewUsageFlushService constructs the usage flush background service.
func NewUsageFlushService(d *UsageFlushDeps) lifecycle.Runner {
	must.NotNil("d", d)
	must.NotNil("d.Flusher", d.Flusher)
	must.NotNil("d.Tracker", d.Tracker)
	must.NotNil("d.Fleet", d.Fleet)
	return &usageFlushService{
		flusher: d.Flusher,
		tracker: d.Tracker,
		fleet:   d.Fleet,
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

// flushTick runs a single flush+metrics cycle. When Redis counters are
// configured, wraps the flush in an advisory lock so only one instance
// performs the destructive GETSET.
func (s *usageFlushService) flushTick(ctx context.Context) {
	// Outside the advisory lock: the byte deltas are this instance's own, so
	// every instance flushes its own set. Skipping them on a lost lock would
	// leave bytes_used short of what this instance wrote.
	if err := s.flusher.FlushQuota(ctx); err != nil && !errors.Is(err, core.ErrDBUnavailable) {
		s.log.ErrorContext(ctx, "quota flush failed", "error", err)
	}

	if s.flusher.RedisCounterConfigured() {
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
	if err := s.flusher.FlushUsage(ctx); err != nil && !errors.Is(err, core.ErrDBUnavailable) {
		s.log.ErrorContext(ctx, "counter flush failed", "error", err)
	}
	if err := s.fleet.UpdateQuotaMetrics(ctx); err != nil && !errors.Is(err, core.ErrDBUnavailable) {
		s.log.ErrorContext(ctx, "quota metrics refresh failed", "error", err)
	}
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
			deleted, failed := manager.ProcessRules(ctx, cfg.Rules, nil)
			if deleted > 0 || failed > 0 {
				log.InfoContext(ctx, "expiration completed",
					"deleted", deleted, "failed", failed)
				event.Publish(event.LifecycleCompleted, "", map[string]any{
					"deleted": deleted,
					"failed":  failed,
				})
			}
			if failed > 0 {
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
