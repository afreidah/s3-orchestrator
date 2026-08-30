// Package usage owns the per-backend usage accounting that needs the metadata
// store: flushing the in-memory counters every backend call increments, and
// recomputing the stored byte total when the incrementally maintained one has
// drifted from the object ledger.
//
// The tracker in internal/counter owns the numbers themselves. This package
// owns when they reach the database and what corrects them when they are
// wrong, which is why it holds a store and the tracker does not. It is
// separate from infra.BackendRuntime for the same reason in reverse: the
// runtime stays free of any store dependency so every worker can reuse it
// through a role interface without dragging persistence along.
package usage
