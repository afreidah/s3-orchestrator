// -------------------------------------------------------------------------------
// LocalCounterBackend - In-Memory Atomic Usage Counters
//
// Author: Alex Freidah
//
// Implements Backend using atomic counters in local memory, one set per
// backend and period. This is the default backend when Redis is not
// configured, and the Redis backend's fallback. Each instance keeps its own
// counters and flushes them to the database on the usage flush tick.
// -------------------------------------------------------------------------------

package counter

import (
	"context"
	"slices"
	"sync/atomic"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// periodKey names one workload's counters on one backend in one period. Keying
// by period means a charge is flushed into the month it was made in, however
// late the flush.
type periodKey struct {
	period   string
	backend  string
	workload string
}

// localCounters holds atomic counters for one periodKey. Pool names come from
// config, so a pool's entry is created the first time it is charged.
type localCounters struct {
	apiRequests  atomic.Int64
	egressBytes  atomic.Int64
	ingressBytes atomic.Int64
	pools        Registry[string, atomic.Int64]
}

// addTo adds the counters into u. Reading into a caller's value rather than
// building a new one is what keeps a limit check from allocating.
func (c *localCounters) addTo(u *core.Usage) {
	u.APIRequests += c.apiRequests.Load()
	u.EgressBytes += c.egressBytes.Load()
	u.IngressBytes += c.ingressBytes.Load()
	c.pools.Range(func(name string, p *atomic.Int64) {
		u.AddPool(name, p.Load())
	})
}

// addStat adds the request and byte totals.
func (c *localCounters) addStat(api, egress, ingress int64) {
	c.apiRequests.Add(api)
	c.egressBytes.Add(egress)
	c.ingressBytes.Add(ingress)
}

// LocalCounterBackend stores per-backend usage counters in local atomic
// counters. A charge for a backend not named at construction is dropped, since
// nothing would ever flush it. Safe for concurrent use.
type LocalCounterBackend struct {
	backends []string
	counters Registry[periodKey, localCounters]
}

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// NewLocalCounterBackend creates a local counter backend for the given backend
// names.
func NewLocalCounterBackend(backendNames []string) *LocalCounterBackend {
	return &LocalCounterBackend{backends: backendNames}
}

// -------------------------------------------------------------------------
// COUNTER BACKEND IMPLEMENTATION
// -------------------------------------------------------------------------

// Backends returns the list of backend names this counter tracks.
func (l *LocalCounterBackend) Backends() []string {
	return l.backends
}

// Charge adds one charge to the backend's counters for the current period.
func (l *LocalCounterBackend) Charge(backend string, c Charge) {
	if c.empty() || !slices.Contains(l.backends, backend) {
		return
	}
	lc := l.counters.Get(periodKey{period: CurrentPeriod(), backend: backend, workload: c.Workload})
	lc.addStat(c.APIRequests, c.EgressBytes, c.IngressBytes)
	if c.PoolN <= 0 {
		return
	}
	for i := range c.Pools {
		lc.pools.Get(c.Pools[i].Name).Add(c.PoolN)
	}
}

// Load adds the backend's unflushed counters for the current period, across
// every workload, into u.
func (l *LocalCounterBackend) Load(backend string, u *core.Usage) {
	period := CurrentPeriod()
	l.counters.Range(func(k periodKey, lc *localCounters) {
		if k.period == period && k.backend == backend {
			lc.addTo(u)
		}
	})
}

// Take empties the counters and returns them, one batch per backend and period
// that was charged. The batches carry no ID: they exist only in this process.
func (l *LocalCounterBackend) Take(_ context.Context) []core.UsageBatch {
	type batchKey struct{ period, backend string }
	grouped := make(map[batchKey]core.WorkloadUsage)
	for k, lc := range l.counters.SwapAll() {
		var u core.Usage
		lc.addTo(&u)
		if u.IsZero() {
			continue
		}
		bk := batchKey{k.period, k.backend}
		if grouped[bk] == nil {
			grouped[bk] = make(core.WorkloadUsage)
		}
		grouped[bk][k.workload] = u
	}
	batches := make([]core.UsageBatch, 0, len(grouped))
	for bk, w := range grouped {
		batches = append(batches, core.UsageBatch{Backend: bk.backend, Period: bk.period, Workloads: w})
	}
	return batches
}

// Settle puts a batch the store did not apply back into its period, so the
// next flush carries it again.
func (l *LocalCounterBackend) Settle(_ context.Context, b *core.UsageBatch, applied bool) {
	if applied {
		return
	}
	for workload, u := range b.Workloads {
		lc := l.counters.Get(periodKey{period: b.Period, backend: b.Backend, workload: workload})
		lc.addStat(u.APIRequests, u.EgressBytes, u.IngressBytes)
		for name, n := range u.Pools {
			lc.pools.Get(name).Add(n)
		}
	}
}
