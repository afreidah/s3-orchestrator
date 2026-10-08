// -------------------------------------------------------------------------------
// Usage Service - Counter Flush and Drift Reconcile
//
// Author: Alex Freidah
//
// The two passes over per-backend usage accounting that touch the metadata
// store: flushing accumulated in-memory counters into it, and recomputing the
// stored byte total from the object ledger when the counter has drifted.
//
// Both need the tracker, the store and the drain state together, which is what
// makes this a type rather than a pair of free functions: the flush cadence and
// the drain skip set are orchestration the tracker has no business knowing.
// -------------------------------------------------------------------------------

package usage

import (
	"context"
	"fmt"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Stores is the persistence surface the two passes need: the flush writes the
// deltas, the reconcile recomputes the total they accumulate into.
type Stores interface {
	core.QuotaStore
	core.UsageFlusher
}

// Deps groups the constructor parameters.
type Deps struct {
	Usage  *counter.UsageTracker
	Quota  *counter.QuotaTracker
	Stores Stores
}

// Service flushes usage counters to the store and reconciles the drift the
// incremental counter accumulates. The flush configuration lives here because
// it is hot-reloadable and this is what reads it.
//
// Holds an atomic config value, so it must not be copied after construction.
type Service struct {
	usage  *counter.UsageTracker
	quota  *counter.QuotaTracker
	stores Stores
	cfg    syncutil.AtomicConfig[config.UsageFlushConfig]
}

// New constructs the service. Usage, Quota and Stores are required.
func New(d *Deps) *Service {
	must.NotNil("d", d)
	must.NotNil("d.Usage", d.Usage)
	must.NotNil("d.Quota", d.Quota)
	must.NotNil("d.Stores", d.Stores)
	return &Service{usage: d.Usage, quota: d.Quota, stores: d.Stores}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// FlushUsage writes the accumulated in-memory counters to the store.
func (s *Service) FlushUsage(ctx context.Context) error {
	return s.usage.FlushUsage(ctx, s.stores)
}

// ReconcileUsage recomputes each backend's stored byte count from the object
// ledger, correcting drift from any mutation path that missed an adjustment.
// It then refreshes the quota baselines, or admission would keep judging writes
// against the replaced totals.
func (s *Service) ReconcileUsage(ctx context.Context) (map[string]int64, error) {
	adjustments, err := s.stores.ReconcileUsage(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.RefreshQuotaBaselines(ctx); err != nil {
		return adjustments, err
	}
	return adjustments, nil
}

// -------------------------------------------------------------------------
// QUOTA VIEW
// -------------------------------------------------------------------------

// FlushQuota reloads the occupancy snapshot placement ranks against. Despite
// the name nothing is written: byte counters move in the write transactions.
func (s *Service) FlushQuota(ctx context.Context) error {
	return s.RefreshQuotaBaselines(ctx)
}

// RefreshQuotaBaselines reloads each backend's ceiling and occupancy from the
// store into the tracker. Called at startup before the listener opens, so the
// first write is judged against real rows rather than an empty snapshot, and
// after every flush so the deltas just written are not counted twice.
func (s *Service) RefreshQuotaBaselines(ctx context.Context) error {
	usage, err := s.stores.ListBackendQuotaUsage(ctx)
	if err != nil {
		return fmt.Errorf("read backend quota usage: %w", err)
	}
	baselines := make(map[string]core.BackendQuotaUsage, len(usage))
	for _, u := range usage {
		baselines[u.BackendName] = u
	}
	s.quota.SetBaselines(baselines)
	return nil
}

// RedisCounterConfigured reports whether the counters live in Redis, whatever
// their health. The flush then holds an advisory lock even during a Redis
// fallback, because a recovery part-way through an unlocked flush double-counts.
func (s *Service) RedisCounterConfigured() bool {
	_, ok := s.usage.Backend().(*counter.RedisCounterBackend)
	return ok
}

// SetConfig atomically replaces the flush configuration.
func (s *Service) SetConfig(cfg *config.UsageFlushConfig) {
	s.cfg.Store(cfg)
}

// Config returns the current flush configuration, nil until one is stored.
func (s *Service) Config() *config.UsageFlushConfig {
	return s.cfg.Load()
}
