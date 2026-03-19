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
	"math/rand/v2"
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

// Run implements lifecycle.Service. A random jitter of up to half the tick
// interval is applied before the first tick to prevent thundering herd on
// the advisory lock when multiple instances start simultaneously.
func (s *lockedTickerService) Run(ctx context.Context) error {
	// Optional one-time startup pass (e.g. replicator catch-up).
	if s.startup != nil {
		s.runOnce(ctx, s.startup)
	}

	// Stagger the first tick to avoid all instances contending for the
	// advisory lock at the same instant.
	jitter := rand.N(s.interval / 2)
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
	acquired, err := s.store.WithAdvisoryLock(tickCtx, s.lockID,
		func(lockCtx context.Context) error {
			fn(lockCtx)
			return nil
		})
	if err != nil && !errors.Is(err, storage.ErrDBUnavailable) {
		if s.onError != nil {
			s.onError(err)
		} else {
			slog.ErrorContext(ctx, s.name+" failed", "error", err)
		}
	}
	if !acquired {
		slog.DebugContext(ctx, s.name + " skipped, another instance holds the lock")
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
					slog.InfoContext(ctx, "Usage flush interval adjusted", "interval", targetInterval)
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
			slog.ErrorContext(ctx, "Usage flush failed", "error", err)
		}
		if !acquired {
			slog.DebugContext(ctx, "Usage flush skipped, another instance holds the lock")
		}
		return
	}

	// No Redis or Redis in fallback — flush locally without lock.
	s.doFlush(ctx)
}

// doFlush performs the actual flush and quota metric update.
func (s *usageFlushService) doFlush(ctx context.Context) {
	if err := s.manager.FlushUsage(ctx); err != nil && !errors.Is(err, storage.ErrDBUnavailable) {
		slog.ErrorContext(ctx, "Failed to flush usage counters", "error", err)
	}
	if err := s.manager.UpdateQuotaMetrics(ctx); err != nil && !errors.Is(err, storage.ErrDBUnavailable) {
		slog.ErrorContext(ctx, "Failed to update quota metrics", "error", err)
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
			manager.MultipartManager.CleanupStaleMultipartUploads(ctx, 24*time.Hour)
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
			processed, failed := manager.CleanupWorker.ProcessCleanupQueue(ctx)
			if processed > 0 || failed > 0 {
				slog.InfoContext(ctx, "Cleanup queue processed", "processed", processed, "failed", failed)
			}
		},
	}
}

func newRebalancerService(manager *storage.BackendManager, store storage.MetadataStore) *lockedTickerService {
	interval := 6 * time.Hour
	if rcfg := manager.Rebalancer.Config(); rcfg != nil && rcfg.Interval > 0 {
		interval = rcfg.Interval
	}
	return &lockedTickerService{
		store:    store,
		interval: interval,
		lockID:   storage.LockRebalancer,
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
			if err != nil && !errors.Is(err, storage.ErrDBUnavailable) {
				slog.ErrorContext(ctx, "Rebalance failed", "error", err)
			} else if moved > 0 {
				slog.InfoContext(ctx, "Rebalance completed", "objects_moved", moved)
				if err := manager.UpdateQuotaMetrics(ctx); err != nil {
					slog.WarnContext(ctx, "Failed to update quota metrics after rebalance", "error", err)
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
				slog.InfoContext(ctx, "Lifecycle expiration completed",
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

func newOverReplicationService(manager *storage.BackendManager, store storage.MetadataStore) *lockedTickerService {
	interval := 5 * time.Minute
	if rcfg := manager.OverReplicationCleaner.Config(); rcfg != nil && rcfg.WorkerInterval > 0 {
		interval = rcfg.WorkerInterval
	}
	return &lockedTickerService{
		store:    store,
		interval: interval,
		lockID:   storage.LockOverReplication,
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
			if err != nil && !errors.Is(err, storage.ErrDBUnavailable) {
				slog.ErrorContext(ctx, "Over-replication cleanup failed", "error", err)
			} else if removed > 0 {
				slog.InfoContext(ctx, "Over-replication cleanup completed", "copies_removed", removed)
				if err := manager.UpdateQuotaMetrics(ctx); err != nil {
					slog.WarnContext(ctx, "Failed to update quota metrics after over-replication cleanup", "error", err)
				}
			}
		},
	}
}

func newReplicatorService(manager *storage.BackendManager, store storage.MetadataStore) *lockedTickerService {
	replicateWork := func(ctx context.Context) {
		rcfg := manager.Replicator.Config()
		if rcfg == nil {
			return
		}
		created, err := manager.Replicator.Replicate(ctx, *rcfg)
		if err != nil && !errors.Is(err, storage.ErrDBUnavailable) {
			slog.ErrorContext(ctx, "Replication failed", "error", err)
		} else if created > 0 {
			slog.InfoContext(ctx, "Replication completed", "copies_created", created)
			if err := manager.UpdateQuotaMetrics(ctx); err != nil {
				slog.WarnContext(ctx, "Failed to update quota metrics after replication", "error", err)
			}
		}
	}

	interval := 5 * time.Minute
	if rcfg := manager.Replicator.Config(); rcfg != nil && rcfg.WorkerInterval > 0 {
		interval = rcfg.WorkerInterval
	}
	return &lockedTickerService{
		store:    store,
		interval: interval,
		lockID:   storage.LockReplicator,
		name:     "Replication",
		shouldRun: func() bool {
			rcfg := manager.Replicator.Config()
			return rcfg != nil && rcfg.Factor > 1
		},
		startup:  replicateWork,
		work:     replicateWork,
	}
}
