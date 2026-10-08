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

import "github.com/afreidah/s3-orchestrator/internal/store/core"

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

// Backend abstracts the storage of per-backend usage deltas: three fixed
// counters per backend - API requests, egress bytes, ingress bytes - plus a
// keyed set of request pools. Implementations must be safe for concurrent use.
// The All and Pools variants let an implementation pipeline several round
// trips. ChargePools is the allocation-free form of AddPools for one amount
// across several pools.
type Backend interface {
	Backends() []string
	Add(backend, field string, delta int64)
	Load(backend, field string) int64
	Swap(backend, field string) int64 // returns the value before the reset

	AddAll(backend string, apiReqs, egress, ingress int64)
	LoadAll(backend string) LoadAllResult

	AddPools(backend string, deltas map[string]int64)
	ChargePools(backend string, pools []core.RequestPool, n int64)
	LoadPool(backend, pool string) int64
	SwapPools(backend string) map[string]int64 // returns the values before the reset
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
