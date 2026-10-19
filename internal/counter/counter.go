// -------------------------------------------------------------------------------
// Backend - Abstraction for Usage Counter Storage
//
// Author: Alex Freidah
//
// Defines the Backend interface that abstracts per-backend usage counter
// storage. Two implementations exist: LocalCounterBackend (in-memory atomics,
// default) and RedisCounterBackend (shared Redis counters for multi-instance
// deployments). The UsageTracker calls this interface instead of touching
// atomics directly, allowing transparent backend swapping.
// -------------------------------------------------------------------------------

package counter

//go:generate mockgen -destination=mock_counter_test.go -package=counter github.com/afreidah/s3-orchestrator/internal/counter Backend

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// INTERFACE
// -------------------------------------------------------------------------

// Backend abstracts the storage of per-backend usage counters: the request and
// byte totals plus a keyed set of request pools, kept per backend and per
// period. Implementations must be safe for concurrent use.
//
// Charge records one charge in the current period. Load adds the current
// period's unflushed counters into u, so the limit check that calls it on
// every request can reuse one value. Take hands this process's counters over
// for a flush, one batch per backend and period, and Settle tells the backend
// whether the store applied a batch; a batch that was not applied is kept and
// flushed again.
type Backend interface {
	Backends() []string
	Charge(backend string, c Charge)
	Load(backend string, u *core.Usage)
	Take(ctx context.Context) []core.UsageBatch
	Settle(ctx context.Context, b *core.UsageBatch, applied bool)
}

// SharedBackend is a Backend whose counters also live where every instance
// charges them. TakeShared stages those for a flush; any instance may take
// them, because the store applies a staged batch at most once.
type SharedBackend interface {
	Backend
	TakeShared(ctx context.Context) []core.UsageBatch
}

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Charge is what one recorded call adds: the request and byte totals, and
// PoolN on each of Pools. A pool listed twice is charged twice.
type Charge struct {
	APIRequests  int64
	EgressBytes  int64
	IngressBytes int64
	Pools        []core.RequestPool
	PoolN        int64
}

// empty reports whether the charge adds nothing.
func (c Charge) empty() bool {
	return c.APIRequests <= 0 && c.EgressBytes <= 0 && c.IngressBytes <= 0 &&
		(len(c.Pools) == 0 || c.PoolN <= 0)
}
