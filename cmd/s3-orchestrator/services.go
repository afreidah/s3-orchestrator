// -------------------------------------------------------------------------------
// Background Service Definitions
//
// Author: Alex Freidah
//
// Service types for the lifecycle manager. Each wraps one of the four periodic
// background tasks (usage flush, multipart cleanup, rebalancer, replicator)
// behind the lifecycle.Service interface.
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
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
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
}

func (s *multipartCleanupService) Run(ctx context.Context) error {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			tickCtx := audit.WithRequestID(ctx, audit.NewID())
			s.manager.CleanupStaleMultipartUploads(tickCtx, 24*time.Hour)
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
			moved, err := s.manager.Rebalance(ctx, *rcfg)
			if err != nil && !errors.Is(err, storage.ErrDBUnavailable) {
				slog.Error("Rebalance failed", "error", err)
			} else if moved > 0 {
				slog.Info("Rebalance completed", "objects_moved", moved)
				if err := s.manager.UpdateQuotaMetrics(ctx); err != nil {
					slog.Warn("Failed to update quota metrics after rebalance", "error", err)
				}
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
}

func (s *replicatorService) Run(ctx context.Context) error {
	// Run once at startup to catch up on pending replicas
	rcfg := s.manager.ReplicationConfig()
	if rcfg != nil && rcfg.Factor > 1 {
		created, err := s.manager.Replicate(ctx, *rcfg)
		if err != nil && !errors.Is(err, storage.ErrDBUnavailable) {
			slog.Error("Replication startup run failed", "error", err)
		} else if created > 0 {
			slog.Info("Replication startup run completed", "copies_created", created)
			if err := s.manager.UpdateQuotaMetrics(ctx); err != nil {
				slog.Warn("Failed to update quota metrics after replication startup", "error", err)
			}
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
			created, err := s.manager.Replicate(ctx, *rcfg)
			if err != nil && !errors.Is(err, storage.ErrDBUnavailable) {
				slog.Error("Replication failed", "error", err)
			} else if created > 0 {
				slog.Info("Replication completed", "copies_created", created)
				if err := s.manager.UpdateQuotaMetrics(ctx); err != nil {
					slog.Warn("Failed to update quota metrics after replication", "error", err)
				}
			}
		case <-ctx.Done():
			return nil
		}
	}
}
