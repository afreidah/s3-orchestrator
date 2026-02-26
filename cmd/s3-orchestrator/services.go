// -------------------------------------------------------------------------------
// Background Service Definitions
//
// Author: Alex Freidah
//
// Service types for the lifecycle manager. Each wraps one of the four periodic
// background tasks (usage flush, multipart cleanup, rebalancer, replicator)
// behind the lifecycle.Service interface. Background tasks that must not run
// concurrently across instances use PostgreSQL advisory locks.
// -------------------------------------------------------------------------------

package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/audit"
	"github.com/afreidah/s3-orchestrator/internal/storage"
)

// -------------------------------------------------------------------------
// USAGE FLUSH
// -------------------------------------------------------------------------

type usageFlushService struct {
	manager *storage.BackendManager
}

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
			if err := s.manager.FlushUsage(tickCtx); err != nil && !errors.Is(err, storage.ErrDBUnavailable) {
				slog.Error("Failed to flush usage counters", "error", err)
			}
			if err := s.manager.UpdateQuotaMetrics(tickCtx); err != nil && !errors.Is(err, storage.ErrDBUnavailable) {
				slog.Error("Failed to update quota metrics", "error", err)
			}

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

// -------------------------------------------------------------------------
// MULTIPART CLEANUP
// -------------------------------------------------------------------------

type multipartCleanupService struct {
	manager *storage.BackendManager
	store   storage.MetadataStore
}

func (s *multipartCleanupService) Run(ctx context.Context) error {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			tickCtx := audit.WithRequestID(ctx, audit.NewID())
			acquired, err := s.store.WithAdvisoryLock(tickCtx, storage.LockMultipartCleanup,
				func(lockCtx context.Context) error {
					s.manager.CleanupStaleMultipartUploads(lockCtx, 24*time.Hour)
					return nil
				})
			if err != nil {
				slog.Error("Multipart cleanup failed", "error", err)
			}
			if !acquired {
				slog.Debug("Multipart cleanup skipped, another instance holds the lock")
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// -------------------------------------------------------------------------
// CLEANUP QUEUE
// -------------------------------------------------------------------------

type cleanupQueueService struct {
	manager *storage.BackendManager
	store   storage.MetadataStore
}

func (s *cleanupQueueService) Run(ctx context.Context) error {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			tickCtx := audit.WithRequestID(ctx, audit.NewID())
			acquired, err := s.store.WithAdvisoryLock(tickCtx, storage.LockCleanupQueue,
				func(lockCtx context.Context) error {
					processed, failed := s.manager.ProcessCleanupQueue(lockCtx)
					if processed > 0 || failed > 0 {
						slog.Info("Cleanup queue processed", "processed", processed, "failed", failed)
					}
					return nil
				})
			if err != nil {
				slog.Error("Cleanup queue processing failed", "error", err)
			}
			if !acquired {
				slog.Debug("Cleanup queue skipped, another instance holds the lock")
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// -------------------------------------------------------------------------
// REBALANCER
// -------------------------------------------------------------------------

type rebalancerService struct {
	manager *storage.BackendManager
	store   storage.MetadataStore
}

func (s *rebalancerService) Run(ctx context.Context) error {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			rcfg := s.manager.RebalanceConfig()
			if rcfg == nil || !rcfg.Enabled {
				continue
			}
			tickCtx := audit.WithRequestID(ctx, audit.NewID())
			acquired, err := s.store.WithAdvisoryLock(tickCtx, storage.LockRebalancer,
				func(lockCtx context.Context) error {
					moved, err := s.manager.Rebalance(lockCtx, *rcfg)
					if err != nil && !errors.Is(err, storage.ErrDBUnavailable) {
						slog.Error("Rebalance failed", "error", err)
					} else if moved > 0 {
						slog.Info("Rebalance completed", "objects_moved", moved)
						if err := s.manager.UpdateQuotaMetrics(lockCtx); err != nil {
							slog.Warn("Failed to update quota metrics after rebalance", "error", err)
						}
					}
					return nil
				})
			if err != nil && !errors.Is(err, storage.ErrDBUnavailable) {
				slog.Error("Rebalance lock acquisition failed", "error", err)
			}
			if !acquired {
				slog.Debug("Rebalance skipped, another instance holds the lock")
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// -------------------------------------------------------------------------
// REPLICATOR
// -------------------------------------------------------------------------

type replicatorService struct {
	manager *storage.BackendManager
	store   storage.MetadataStore
}

func (s *replicatorService) Run(ctx context.Context) error {
	// Run once at startup to catch up on pending replicas
	rcfg := s.manager.ReplicationConfig()
	if rcfg != nil && rcfg.Factor > 1 {
		startCtx := audit.WithRequestID(ctx, audit.NewID())
		acquired, err := s.store.WithAdvisoryLock(startCtx, storage.LockReplicator,
			func(lockCtx context.Context) error {
				created, err := s.manager.Replicate(lockCtx, *rcfg)
				if err != nil && !errors.Is(err, storage.ErrDBUnavailable) {
					slog.Error("Replication startup run failed", "error", err)
				} else if created > 0 {
					slog.Info("Replication startup run completed", "copies_created", created)
					if err := s.manager.UpdateQuotaMetrics(lockCtx); err != nil {
						slog.Warn("Failed to update quota metrics after replication startup", "error", err)
					}
				}
				return nil
			})
		if err != nil && !errors.Is(err, storage.ErrDBUnavailable) {
			slog.Error("Replication startup lock failed", "error", err)
		}
		if !acquired {
			slog.Debug("Replication startup skipped, another instance holds the lock")
		}
	}

	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			rcfg := s.manager.ReplicationConfig()
			if rcfg == nil || rcfg.Factor <= 1 {
				continue
			}
			tickCtx := audit.WithRequestID(ctx, audit.NewID())
			acquired, err := s.store.WithAdvisoryLock(tickCtx, storage.LockReplicator,
				func(lockCtx context.Context) error {
					created, err := s.manager.Replicate(lockCtx, *rcfg)
					if err != nil && !errors.Is(err, storage.ErrDBUnavailable) {
						slog.Error("Replication failed", "error", err)
					} else if created > 0 {
						slog.Info("Replication completed", "copies_created", created)
						if err := s.manager.UpdateQuotaMetrics(lockCtx); err != nil {
							slog.Warn("Failed to update quota metrics after replication", "error", err)
						}
					}
					return nil
				})
			if err != nil && !errors.Is(err, storage.ErrDBUnavailable) {
				slog.Error("Replication lock acquisition failed", "error", err)
			}
			if !acquired {
				slog.Debug("Replication skipped, another instance holds the lock")
			}
		case <-ctx.Done():
			return nil
		}
	}
}
