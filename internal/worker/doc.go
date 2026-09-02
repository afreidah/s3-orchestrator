// Package worker holds the background passes that run on a tick rather than on
// a request: replication, rebalance, cleanup and its dead-letter queue, the
// pending-intent reaper, integrity scrub and backfill, and reconcile. Each owns
// one unit of work, reports it as a WorkSummary, and takes only the narrow store
// roles it calls.
package worker
