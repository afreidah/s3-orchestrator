// -------------------------------------------------------------------------------
// Background Service Definitions - Manager-Coupled
//
// Author: Alex Freidah
//
// Two background services whose run-loop semantics live in DI because
// they read state from *proxy.BackendManager via consumer interfaces
// (usageFlushOps / lifecycleOps) declared in service_interfaces.go:
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
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
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
	manager usageFlushOps
	locker  tickrunner.AdvisoryLocker
	log     *slog.Logger
}

// NewUsageFlushService constructs the usage flush background service.
func NewUsageFlushService(manager usageFlushOps, locker tickrunner.AdvisoryLocker) lifecycle.Runner {
	return &usageFlushService{
		manager: manager,
		locker:  locker,
		log:     tickrunner.ComponentLogger("usage_flush"),
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
	cfg := s.manager.UsageFlushConfig()
	if cfg == nil {
		return current
	}
	target := cfg.Interval
	if cfg.AdaptiveEnabled && s.manager.NearUsageLimit(cfg.AdaptiveThreshold) {
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
// LIFECYCLE
// -------------------------------------------------------------------------

// NewLifecycleService constructs the lifecycle-expiration background
// service. Lives in DI (rather than next to a worker) because the work
// surface is on *proxy.BackendManager itself (via the lifecycleOps
// consumer interface) - there is no dedicated worker type.
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
			cfg := manager.LifecycleConfig()
			return cfg != nil && len(cfg.Rules) > 0
		},
		Work: func(ctx context.Context) error {
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
		OnError: func(err error) {
			log.ErrorContext(context.Background(), "expiration failed", "error", err)
			telemetry.LifecycleRunsTotal.WithLabelValues("error").Inc()
		},
	})
}
