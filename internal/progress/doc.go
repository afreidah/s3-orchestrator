// Package progress defines the transport-agnostic observer that long-running
// operations - backfill, reconcile, scrub, replicate, purge - emit through to
// report per-item progress. Each unit of work brackets a start and an end event
// carrying the item label, outcome and duration.
//
// A leaf package with no app dependencies, so the worker and proxy layers can
// emit and the transport layer can render without a cycle.
package progress
