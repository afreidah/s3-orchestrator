// Package proxy is the request-path orchestration layer that fronts the
// configured backend fleet. Its BackendManager ties together the read and
// write paths, the in-memory runtime (backend registry, routing order, health
// and drain state), and the store-coupled operations that admin and background
// workers drive: dashboard aggregation, usage flush and reconcile, backend
// add/remove, and object-location lookups.
//
// BackendManager is deliberately a thin orchestrator - it forwards to focused
// collaborators (the write coordinator's shared move/delete primitives, the
// runtime, the dashboard aggregator, the drain manager) rather than
// reimplementing their logic - so the fleet's quota accounting, cache
// invalidation, and cleanup enqueue all funnel through one path. Lifecycle
// expiration and the automatic-cleanup helpers that live alongside it reuse the
// same store operations so quota and the object ledger never diverge.
package proxy
