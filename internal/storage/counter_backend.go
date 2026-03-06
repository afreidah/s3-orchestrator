// -------------------------------------------------------------------------------
// CounterBackend - Abstraction for Usage Counter Storage
//
// Author: Alex Freidah
//
// Defines the CounterBackend interface that abstracts per-backend usage counter
// storage. Two implementations exist: LocalCounterBackend (in-memory atomics,
// default) and RedisCounterBackend (shared Redis counters for multi-instance
// deployments). The UsageTracker calls this interface instead of touching
// atomics directly, allowing transparent backend swapping.
// -------------------------------------------------------------------------------

package storage

// -------------------------------------------------------------------------
// FIELD CONSTANTS
// -------------------------------------------------------------------------

// Counter field names used as keys in CounterBackend operations.
const (
	FieldAPIRequests  = "api_requests"
	FieldEgressBytes  = "egress_bytes"
	FieldIngressBytes = "ingress_bytes"
)

// -------------------------------------------------------------------------
// INTERFACE
// -------------------------------------------------------------------------

// CounterBackend abstracts the storage of per-backend usage deltas. Each
// backend (identified by name) tracks three counters: API requests, egress
// bytes, and ingress bytes. Implementations must be safe for concurrent use.
type CounterBackend interface {
	// Backends returns the names of all tracked backends.
	Backends() []string

	// Add increments a single counter field for the given backend.
	Add(backend, field string, delta int64)

	// Load returns the current value of a single counter field.
	Load(backend, field string) int64

	// Swap atomically reads and resets a single counter field, returning
	// the value immediately before the reset.
	Swap(backend, field string) int64

	// AddAll increments all three counter fields for the given backend in
	// a single call. Implementations may pipeline the operations.
	AddAll(backend string, apiReqs, egress, ingress int64)

	// LoadAll reads all three counter fields for the given backend in a
	// single call. Implementations may pipeline the operations.
	LoadAll(backend string) LoadAllResult
}

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// LoadAllResult holds the values returned by CounterBackend.LoadAll.
type LoadAllResult struct {
	APIRequests  int64
	EgressBytes  int64
	IngressBytes int64
}
