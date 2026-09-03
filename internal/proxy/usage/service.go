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

// DrainReader reports which backends have finished draining. Nil-able, and a
// deployment without drain skips nothing.
type DrainReader interface {
	CompletedBackends() map[string]bool
}

// Deps groups the constructor parameters. Drain is the only optional one.
type Deps struct {
	Usage  *counter.UsageTracker
	Stores Stores
	Drain  DrainReader
}

// Service flushes usage counters to the store and reconciles the drift the
// incremental counter accumulates. The flush configuration lives here because
// it is hot-reloadable and this is what reads it.
//
// Holds an atomic config value, so it must not be copied after construction.
type Service struct {
	usage  *counter.UsageTracker
	stores Stores
	drain  DrainReader
	cfg    syncutil.AtomicConfig[config.UsageFlushConfig]
}

// New constructs the service. Usage and Stores are required; a nil Drain
// leaves the flush skipping nothing, which is what a deployment that never
// drains a backend wants.
func New(d *Deps) *Service {
	must.NotNil("d", d)
	must.NotNil("d.Usage", d.Usage)
	must.NotNil("d.Stores", d.Stores)
	return &Service{usage: d.Usage, stores: d.Stores, drain: d.Drain}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// FlushUsage writes the accumulated in-memory counters to the store. Backends
// that have finished draining are skipped: their rows, backend_usage included,
// are already gone, so flushing them would write back what the drain removed.
func (s *Service) FlushUsage(ctx context.Context) error {
	var skip map[string]bool
	if s.drain != nil {
		skip = s.drain.CompletedBackends()
	}
	return s.usage.FlushUsage(ctx, s.stores, skip)
}

// ReconcileUsage recomputes each backend's stored byte count from the object
// ledger. The counter is otherwise maintained incrementally and drifts
// permanently once any mutation path misses an adjustment, so this is the
// self-heal rather than a diagnostic.
func (s *Service) ReconcileUsage(ctx context.Context) (map[string]int64, error) {
	return s.stores.ReconcileUsage(ctx)
}

// RedisCounterConfigured reports whether the counters live in Redis, whatever
// their health. The flush holds an advisory lock when they do, and has to keep
// holding it while Redis is being fallen back on: a recovery part-way through
// an unlocked flush double-counts.
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
