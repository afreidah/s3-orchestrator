// -------------------------------------------------------------------------------
// LocalCounterBackend - In-Memory Atomic Usage Counters
//
// Author: Alex Freidah
//
// Implements Backend using atomic counters in local memory, one set per
// backend, hourly bucket and workload. This is the default backend when Redis
// is not configured, and the Redis backend's fallback. Each instance keeps its
// own counters and flushes them to the database on the usage flush tick.
// -------------------------------------------------------------------------------

package counter

import (
	"context"
	"slices"
	"sync/atomic"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// bucketKey names one workload's counters on one backend in one bucket, so a
// charge is flushed into the hour it was made in however late the flush.
type bucketKey struct {
	bucket   time.Time
	backend  string
	workload string
}

// localCounters holds atomic counters for one bucketKey. Pool names come from
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
	counters Registry[bucketKey, localCounters]
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

// Charge adds one charge to the backend's counters for bucket. A charge for a
// backend not named at construction is dropped.
func (l *LocalCounterBackend) Charge(backend string, bucket time.Time, c Charge) {
	if c.empty() || !slices.Contains(l.backends, backend) {
		return
	}
	lc := l.counters.Get(bucketKey{bucket: bucket, backend: backend, workload: c.Workload})
	lc.addStat(c.APIRequests, c.EgressBytes, c.IngressBytes)
	if c.PoolN <= 0 {
		return
	}
	for i := range c.Pools {
		lc.pools.Get(c.Pools[i].Name).Add(c.PoolN)
	}
}

// Load adds the backend's unflushed counters in buckets from since onward,
// across every workload, into u.
func (l *LocalCounterBackend) Load(backend string, since time.Time, u *core.Usage) {
	l.counters.Range(func(k bucketKey, lc *localCounters) {
		if k.backend == backend && !k.bucket.Before(since) {
			lc.addTo(u)
		}
	})
}

// Take empties the counters and returns them, one batch per backend that was
// charged. The batches carry no ID: they exist only in this process.
func (l *LocalCounterBackend) Take(_ context.Context) []core.UsageBatch {
	grouped := make(map[string]core.BucketUsage)
	for k, lc := range l.counters.SwapAll() {
		var u core.Usage
		lc.addTo(&u)
		if u.IsZero() {
			continue
		}
		if grouped[k.backend] == nil {
			grouped[k.backend] = make(core.BucketUsage)
		}
		grouped[k.backend].Add(k.bucket, k.workload, u)
	}
	batches := make([]core.UsageBatch, 0, len(grouped))
	for backend, buckets := range grouped {
		batches = append(batches, core.UsageBatch{Backend: backend, Buckets: buckets})
	}
	return batches
}

// Settle puts a batch the store did not apply back into its period, so the
// next flush carries it again.
func (l *LocalCounterBackend) Settle(_ context.Context, b *core.UsageBatch, applied bool) {
	if applied {
		return
	}
	_ = b.Each(func(bucket time.Time, workload string, u *core.Usage) error {
		lc := l.counters.Get(bucketKey{bucket: bucket, backend: b.Backend, workload: workload})
		lc.addStat(u.APIRequests, u.EgressBytes, u.IngressBytes)
		for name, n := range u.Pools {
			lc.pools.Get(name).Add(n)
		}
		return nil
	})
}
