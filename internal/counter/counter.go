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

// Package counter provides usage tracking with per-backend atomic counters
// and monthly limit enforcement. Supports local in-memory counters and
// Redis-backed shared counters for multi-instance deployments.
package counter

//go:generate mockgen -destination=mock_counter_test.go -package=counter github.com/afreidah/s3-orchestrator/internal/counter Backend

// -------------------------------------------------------------------------
// FIELD CONSTANTS
// -------------------------------------------------------------------------

// Counter field names used as keys in Backend operations.
const (
	FieldAPIRequests  = "api_requests"
	FieldEgressBytes  = "egress_bytes"
	FieldIngressBytes = "ingress_bytes"
)

// -------------------------------------------------------------------------
// INTERFACE
// -------------------------------------------------------------------------

// Backend abstracts the storage of per-backend usage deltas. Each
// backend (identified by name) tracks three counters: API requests, egress
// bytes, and ingress bytes. Implementations must be safe for concurrent use.
type Backend interface {
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

	// AddPools increments per-pool request counters, keyed by pool name.
	// Implementations may pipeline the increments.
	AddPools(backend string, deltas map[string]int64)

	// LoadPool returns the current count for a single pool. Admission
	// reads one or two of these per request, so it stays a point read
	// rather than a map allocation.
	LoadPool(backend, pool string) int64

	// SwapPools atomically reads and resets every pool counter for the
	// backend, returning the values immediately before the reset.
	SwapPools(backend string) map[string]int64
}

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// LoadAllResult holds the values returned by Backend.LoadAll.
type LoadAllResult struct {
	APIRequests  int64
	EgressBytes  int64
	IngressBytes int64
}

// Snapshot is one backend's unflushed counters: the three fixed dimensions
// plus every request pool charged since the last reset. Pools are additive,
// so their values do not sum to APIRequests.
type Snapshot struct {
	LoadAllResult
	Pools map[string]int64
}
