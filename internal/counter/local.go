// -------------------------------------------------------------------------------
// LocalCounterBackend - In-Memory Atomic Usage Counters
//
// Author: Alex Freidah
//
// Implements Backend using per-backend atomic.Int64 counters stored in
// local memory. This is the default backend when Redis is not configured. Each
// instance maintains independent counters that are periodically flushed to
// PostgreSQL by the usage flush service.
// -------------------------------------------------------------------------------

package counter

import (
	"sync"
	"sync/atomic"
)

// localCounters holds atomic counters for a single backend's usage deltas.
//
// The three fixed dimensions are named fields because every backend has
// exactly those; request pools are a map because their names come from
// config and change with it. The map is guarded rather than atomic since it
// is only written when a pool is charged for the first time in a period.
type localCounters struct {
	apiRequests  atomic.Int64
	egressBytes  atomic.Int64
	ingressBytes atomic.Int64

	mu    sync.RWMutex
	pools map[string]*atomic.Int64
}

// pool returns the counter for one pool, creating it on first charge.
func (c *localCounters) pool(name string) *atomic.Int64 {
	c.mu.RLock()
	p := c.pools[name]
	c.mu.RUnlock()
	if p != nil {
		return p
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if existing := c.pools[name]; existing != nil {
		return existing
	}
	created := &atomic.Int64{}
	if c.pools == nil {
		c.pools = make(map[string]*atomic.Int64)
	}
	c.pools[name] = created
	return created
}

// poolValues reads every pool counter for this backend.
func (c *localCounters) poolValues() map[string]int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.pools) == 0 {
		return nil
	}
	out := make(map[string]int64, len(c.pools))
	for name, p := range c.pools {
		out[name] = p.Load()
	}
	return out
}

// LocalCounterBackend stores per-backend usage deltas in local atomic
// counters. Safe for concurrent use.
type LocalCounterBackend struct {
	mu       sync.RWMutex
	counters map[string]*localCounters
}

// NewLocalCounterBackend creates a local counter backend pre-initialized with
// the given backend names.
func NewLocalCounterBackend(backendNames []string) *LocalCounterBackend {
	counters := make(map[string]*localCounters, len(backendNames))
	for _, name := range backendNames {
		counters[name] = &localCounters{}
	}
	return &LocalCounterBackend{counters: counters}
}

// -------------------------------------------------------------------------
// COUNTER BACKEND IMPLEMENTATION
// -------------------------------------------------------------------------

// Backends returns the list of backend names this counter tracks.
func (l *LocalCounterBackend) Backends() []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	names := make([]string, 0, len(l.counters))
	for name := range l.counters {
		names = append(names, name)
	}
	return names
}

// Add increments a single counter field for a backend.
func (l *LocalCounterBackend) Add(backend, field string, delta int64) {
	c := l.get(backend)
	if c == nil {
		return
	}
	switch field {
	case FieldAPIRequests:
		c.apiRequests.Add(delta)
	case FieldEgressBytes:
		c.egressBytes.Add(delta)
	case FieldIngressBytes:
		c.ingressBytes.Add(delta)
	}
}

// Load returns the current value of a counter field.
func (l *LocalCounterBackend) Load(backend, field string) int64 {
	c := l.get(backend)
	if c == nil {
		return 0
	}
	switch field {
	case FieldAPIRequests:
		return c.apiRequests.Load()
	case FieldEgressBytes:
		return c.egressBytes.Load()
	case FieldIngressBytes:
		return c.ingressBytes.Load()
	}
	return 0
}

// Swap atomically reads and resets a counter field, returning the old value.
func (l *LocalCounterBackend) Swap(backend, field string) int64 {
	c := l.get(backend)
	if c == nil {
		return 0
	}
	switch field {
	case FieldAPIRequests:
		return c.apiRequests.Swap(0)
	case FieldEgressBytes:
		return c.egressBytes.Swap(0)
	case FieldIngressBytes:
		return c.ingressBytes.Swap(0)
	}
	return 0
}

// AddAll increments all three counter fields (API requests, egress, ingress) atomically.
func (l *LocalCounterBackend) AddAll(backend string, apiReqs, egress, ingress int64) {
	c := l.get(backend)
	if c == nil {
		return
	}
	if apiReqs > 0 {
		c.apiRequests.Add(apiReqs)
	}
	if egress > 0 {
		c.egressBytes.Add(egress)
	}
	if ingress > 0 {
		c.ingressBytes.Add(ingress)
	}
}

// LoadAll returns all three counter values for a backend.
func (l *LocalCounterBackend) LoadAll(backend string) LoadAllResult {
	c := l.get(backend)
	if c == nil {
		return LoadAllResult{}
	}
	return LoadAllResult{
		APIRequests:  c.apiRequests.Load(),
		EgressBytes:  c.egressBytes.Load(),
		IngressBytes: c.ingressBytes.Load(),
	}
}

// SwapAll atomically reads and resets all three counter fields for a backend,
// returning the old values. Each field is independently atomic.
func (l *LocalCounterBackend) SwapAll(backend string) LoadAllResult {
	c := l.get(backend)
	if c == nil {
		return LoadAllResult{}
	}
	return LoadAllResult{
		APIRequests:  c.apiRequests.Swap(0),
		EgressBytes:  c.egressBytes.Swap(0),
		IngressBytes: c.ingressBytes.Swap(0),
	}
}

// SwapAllBackends atomically reads and resets counters for every backend in
// a single operation by swapping the entire map. Returns the old values keyed
// by backend name. This avoids the race where per-backend SwapAll calls allow
// concurrent Add calls to slip between swaps.
func (l *LocalCounterBackend) SwapAllBackends() map[string]Snapshot {
	l.mu.Lock()
	old := l.counters
	fresh := make(map[string]*localCounters, len(old))
	for name := range old {
		fresh[name] = &localCounters{}
	}
	l.counters = fresh
	l.mu.Unlock()

	result := make(map[string]Snapshot, len(old))
	for name, c := range old {
		result[name] = Snapshot{
			LoadAllResult: LoadAllResult{
				APIRequests:  c.apiRequests.Load(),
				EgressBytes:  c.egressBytes.Load(),
				IngressBytes: c.ingressBytes.Load(),
			},
			Pools: c.poolValues(),
		}
	}
	return result
}

// AddPools increments the named pool counters for a backend.
func (l *LocalCounterBackend) AddPools(backend string, deltas map[string]int64) {
	c := l.get(backend)
	if c == nil {
		return
	}
	for name, delta := range deltas {
		if delta > 0 {
			c.pool(name).Add(delta)
		}
	}
}

// LoadPool returns the current count for a single pool.
func (l *LocalCounterBackend) LoadPool(backend, pool string) int64 {
	c := l.get(backend)
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if p := c.pools[pool]; p != nil {
		return p.Load()
	}
	return 0
}

// SwapPools reads and resets every pool counter for a backend.
func (l *LocalCounterBackend) SwapPools(backend string) map[string]int64 {
	c := l.get(backend)
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pools) == 0 {
		return nil
	}
	out := make(map[string]int64, len(c.pools))
	for name, p := range c.pools {
		out[name] = p.Swap(0)
	}
	return out
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// get returns the counters for the named backend, or nil if unknown.
func (l *LocalCounterBackend) get(backend string) *localCounters {
	l.mu.RLock()
	c := l.counters[backend]
	l.mu.RUnlock()
	return c
}
