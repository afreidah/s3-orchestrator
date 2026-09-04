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
	"sync/atomic"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// localCounters holds atomic counters for a single backend's usage deltas.
//
// The three fixed dimensions are named fields because every backend has
// exactly those; request pools live in a registry because their names come
// from config and change with it, so an entry is created the first time a
// pool is charged rather than declared up front.
type localCounters struct {
	apiRequests  atomic.Int64
	egressBytes  atomic.Int64
	ingressBytes atomic.Int64
	pools        Registry[atomic.Int64]
}

// poolValues reads every pool counter for this backend.
func (c *localCounters) poolValues() map[string]int64 {
	entries := c.pools.All()
	if len(entries) == 0 {
		return nil
	}
	out := make(map[string]int64, len(entries))
	for name, p := range entries {
		out[name] = p.Load()
	}
	return out
}

// LocalCounterBackend stores per-backend usage deltas in local atomic
// counters. Safe for concurrent use.
type LocalCounterBackend struct {
	counters *Registry[localCounters]
}

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// NewLocalCounterBackend creates a local counter backend pre-initialized with
// the given backend names.
func NewLocalCounterBackend(backendNames []string) *LocalCounterBackend {
	return &LocalCounterBackend{counters: NewRegistry[localCounters](backendNames...)}
}

// -------------------------------------------------------------------------
// COUNTER BACKEND IMPLEMENTATION
// -------------------------------------------------------------------------

// Backends returns the list of backend names this counter tracks.
func (l *LocalCounterBackend) Backends() []string {
	return l.counters.Keys()
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
	old := l.counters.SwapAll()

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
			c.pools.Get(name).Add(delta)
		}
	}
}

// LoadPool returns the current count for a single pool.
func (l *LocalCounterBackend) LoadPool(backend, pool string) int64 {
	c := l.get(backend)
	if c == nil {
		return 0
	}
	if p := c.pools.Peek(pool); p != nil {
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
	old := c.pools.SwapAll()
	if len(old) == 0 {
		return nil
	}
	out := make(map[string]int64, len(old))
	for name, p := range old {
		out[name] = p.Load()
	}
	return out
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// get returns the counters for the named backend, or nil if unknown.
func (l *LocalCounterBackend) get(backend string) *localCounters {
	return l.counters.Peek(backend)
}
