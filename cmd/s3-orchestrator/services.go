// -------------------------------------------------------------------------------
// Background Service Definitions
//
// Author: Alex Freidah
//
// Service types for the lifecycle manager. Each wraps a periodic background task
// behind the lifecycle.Service interface. Tasks that must not run concurrently
// across instances use PostgreSQL advisory locks via lockedTickerService.
// -------------------------------------------------------------------------------

package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/audit"
	"github.com/afreidah/s3-orchestrator/internal/storage"
	"github.com/afreidah/s3-orchestrator/internal/telemetry"
)

// -------------------------------------------------------------------------
// LOCKED TICKER SERVICE
// -------------------------------------------------------------------------

// lockedTickerService runs a function on a fixed interval under an advisory
// lock. It handles audit context creation, lock acquisition, skip/error
// logging, and context cancellation. Most background workers use this.
type lockedTickerService struct {
	store    storage.MetadataStore
	interval time.Duration
	lockID   int64
	name     string

	// shouldRun is called each tick before acquiring the lock.
	// Return false to skip this tick. Nil means always run.
	shouldRun func() bool

	// startup is called once before the ticker loop starts, under the
	// advisory lock. Nil means no startup pass.
	startup func(ctx context.Context)

	// work is called inside the advisory lock with an audit-tagged context.
	work func(ctx context.Context)

	// onError is called when WithAdvisoryLock returns an error (that is not
	// ErrDBUnavailable). Nil means just log the error with the service name.
	onError func(err error)
}

// Run implements lifecycle.Service.
func (s *lockedTickerService) Run(ctx context.Context) error {
	// Optional one-time startup pass (e.g. replicator catch-up).
	if s.startup != nil {
		s.runOnce(ctx, s.startup)
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
	acquired, err := s.store.WithAdvisoryLock(tickCtx, s.lockID,
		func(lockCtx context.Context) error {
			fn(lockCtx)
			return nil
		})
	if err != nil && !errors.Is(err, storage.ErrDBUnavailable) {
		if s.onError != nil {
			s.onError(err)
		} else {
			slog.Error(s.name+" failed", "error", err)
		}
	}
	if !acquired {
		slog.Debug(s.name + " skipped, another instance holds the lock")
	}
}

// -------------------------------------------------------------------------
// USAGE FLUSH (unique: no advisory lock, adaptive interval)
// -------------------------------------------------------------------------

type usageFlushService struct {
	manager *storage.BackendManager
	store   storage.MetadataStore
}

// Run periodically flushes in-memory usage counters to the database.
// When Redis counters are active, only one instance should flush (GETSET is a
// destructive read), so the flush is wrapped in an advisory lock. When Redis
// is in fallback or not configured, each instance flushes independently.
func (s *usageFlushService) Run(ctx context.Context) error {
	cfg := s.manager.UsageFlushConfig()
	interval := 30 * time.Second
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

			// Adaptive interval adjustment
			cfg = s.manager.UsageFlushConfig()
			if cfg != nil {
				targetInterval := cfg.Interval
				if cfg.AdaptiveEnabled && s.manager.NearUsageLimit(cfg.AdaptiveThreshold) {
					targetInterval = cfg.FastInterval
				}
				if targetInterval != currentInterval {
					ticker.Reset(targetInterval)
					currentInterval = targetInterval
					slog.Info("Usage flush interval adjusted", "interval", targetInterval)
				}
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// flushTick runs a single flush+metrics cycle. When Redis counters are active,
// wraps the flush in an advisory lock so only one instance performs GETSET.
func (s *usageFlushService) flushTick(ctx context.Context) {
	if s.manager.RedisCounterActive() {
		acquired, err := s.store.WithAdvisoryLock(ctx, storage.LockUsageFlush,
			func(lockCtx context.Context) error {
				s.doFlush(lockCtx)
				return nil
			})
		if err != nil && !errors.Is(err, storage.ErrDBUnavailable) {
			slog.Error("Usage flush failed", "error", err)
		}
		if !acquired {
			slog.Debug("Usage flush skipped, another instance holds the lock")
		}
		return
	}

	// No Redis or Redis in fallback — flush locally without lock.
	s.doFlush(ctx)
}

// doFlush performs the actual flush and quota metric update.
func (s *usageFlushService) doFlush(ctx context.Context) {
	if err := s.manager.FlushUsage(ctx); err != nil && !errors.Is(err, storage.ErrDBUnavailable) {
		slog.Error("Failed to flush usage counters", "error", err)
	}
	if err := s.manager.UpdateQuotaMetrics(ctx); err != nil && !errors.Is(err, storage.ErrDBUnavailable) {
		slog.Error("Failed to update quota metrics", "error", err)
	}
}

// -------------------------------------------------------------------------
// SERVICE CONSTRUCTORS
// -------------------------------------------------------------------------

func newMultipartCleanupService(manager *storage.BackendManager, store storage.MetadataStore) *lockedTickerService {
	return &lockedTickerService{
		store:    store,
		interval: 1 * time.Hour,
		lockID:   storage.LockMultipartCleanup,
		name:     "Multipart cleanup",
		work: func(ctx context.Context) {
			manager.CleanupStaleMultipartUploads(ctx, 24*time.Hour)
		},
	}
}

func newCleanupQueueService(manager *storage.BackendManager, store storage.MetadataStore) *lockedTickerService {
	return &lockedTickerService{
		store:    store,
		interval: 1 * time.Minute,
		lockID:   storage.LockCleanupQueue,
		name:     "Cleanup queue",
		work: func(ctx context.Context) {
			processed, failed := manager.ProcessCleanupQueue(ctx)
			if processed > 0 || failed > 0 {
				slog.Info("Cleanup queue processed", "processed", processed, "failed", failed)
			}
		},
	}
}

func newRebalancerService(manager *storage.BackendManager, store storage.MetadataStore) *lockedTickerService {
	interval := 6 * time.Hour
	if rcfg := manager.RebalanceConfig(); rcfg != nil && rcfg.Interval > 0 {
		interval = rcfg.Interval
	}
	return &lockedTickerService{
		store:    store,
		interval: interval,
		lockID:   storage.LockRebalancer,
		name:     "Rebalance",
		shouldRun: func() bool {
			rcfg := manager.RebalanceConfig()
			return rcfg != nil && rcfg.Enabled
		},
		work: func(ctx context.Context) {
			rcfg := manager.RebalanceConfig()
			if rcfg == nil {
				return
			}
			moved, err := manager.Rebalance(ctx, *rcfg)
			if err != nil && !errors.Is(err, storage.ErrDBUnavailable) {
				slog.Error("Rebalance failed", "error", err)
			} else if moved > 0 {
				slog.Info("Rebalance completed", "objects_moved", moved)
				if err := manager.UpdateQuotaMetrics(ctx); err != nil {
					slog.Warn("Failed to update quota metrics after rebalance", "error", err)
				}
			}
		},
	}
}

func newLifecycleService(manager *storage.BackendManager, store storage.MetadataStore) *lockedTickerService {
	return &lockedTickerService{
		store:    store,
		interval: 1 * time.Hour,
		lockID:   storage.LockLifecycle,
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
				slog.Info("Lifecycle expiration completed",
					"deleted", deleted, "failed", failed)
			}
			if failed > 0 {
				telemetry.LifecycleRunsTotal.WithLabelValues("partial").Inc()
			} else {
				telemetry.LifecycleRunsTotal.WithLabelValues("success").Inc()
			}
		},
		onError: func(err error) {
			slog.Error("Lifecycle expiration failed", "error", err)
			telemetry.LifecycleRunsTotal.WithLabelValues("error").Inc()
		},
	}
}

func newReplicatorService(manager *storage.BackendManager, store storage.MetadataStore) *lockedTickerService {
	replicateWork := func(ctx context.Context) {
		rcfg := manager.ReplicationConfig()
		if rcfg == nil {
			return
		}
		created, err := manager.Replicate(ctx, *rcfg)
		if err != nil && !errors.Is(err, storage.ErrDBUnavailable) {
			slog.Error("Replication failed", "error", err)
		} else if created > 0 {
			slog.Info("Replication completed", "copies_created", created)
			if err := manager.UpdateQuotaMetrics(ctx); err != nil {
				slog.Warn("Failed to update quota metrics after replication", "error", err)
			}
		}
	}

	interval := 5 * time.Minute
	if rcfg := manager.ReplicationConfig(); rcfg != nil && rcfg.WorkerInterval > 0 {
		interval = rcfg.WorkerInterval
	}
	return &lockedTickerService{
		store:    store,
		interval: interval,
		lockID:   storage.LockReplicator,
		name:     "Replication",
		shouldRun: func() bool {
			rcfg := manager.ReplicationConfig()
			return rcfg != nil && rcfg.Factor > 1
		},
		startup:  replicateWork,
		work:     replicateWork,
	}
}
