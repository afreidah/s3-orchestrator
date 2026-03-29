// -------------------------------------------------------------------------------
// LocalCounterBackend - In-Memory Atomic Usage Counters
//
// Author: Alex Freidah
//
// Implements CounterBackend using per-backend atomic.Int64 counters stored in
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
type localCounters struct {
	apiRequests  atomic.Int64
	egressBytes  atomic.Int64
	ingressBytes atomic.Int64
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

// Add increments a single counter field for a backend.
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

// Load returns the current value of a counter field.
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
