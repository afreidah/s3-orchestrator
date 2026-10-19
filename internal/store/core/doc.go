// Package core holds the engine-agnostic transactional logic for the metadata
// store. It declares the per-feature seams - TxAdapter (transactional
// primitives) and Runner (transaction opener) -
// that the postgres and sqlite engine packages implement by translating
// sqlc/driver rows into the canonical domain types defined here. Business
// logic in this package operates exclusively on those interfaces and never
// touches a driver-typed value, so both engines share one implementation of
// RecordObject, DeleteObject, PlaceCopy, DeleteObjectLocation, the pending
// claim, and the object/replica orchestration around them.
//
// The central invariant these operations maintain is that each backend's
// striped byte total always equals SUM(object_locations.size_bytes) for that
// backend: every ledger mutation adjusts the stripes in the same transaction,
// and each engine's ReconcileUsage corrects any drift from the ledger.
package core
