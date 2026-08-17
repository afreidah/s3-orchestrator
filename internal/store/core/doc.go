// Package core holds the engine-agnostic transactional logic for the metadata
// store. It declares the per-feature seams - TxAdapter (transactional
// primitives) and Runner (transaction opener) -
// that the postgres and sqlite engine packages implement by translating
// sqlc/driver rows into the canonical domain types defined here. Business
// logic in this package operates exclusively on those interfaces and never
// touches a driver-typed value, so both engines share one implementation of
// RecordObject, DeleteObject, MoveObjectLocation, RemoveExcessCopy,
// ReconcileUsage, and the object/replica orchestration around them.
//
// The central invariant these operations maintain is that each backend's
// incrementally maintained backend_quotas.bytes_used always equals
// SUM(object_locations.size_bytes) for that backend: every ledger mutation
// adjusts the counter in the same transaction, and ReconcileUsage recomputes
// it from the ledger to correct any drift.
package core
