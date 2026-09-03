// Package instanceid produces a stable per-process identifier that the
// cleanup worker writes into cleanup_queue.claimed_by for operator
// visibility into which orchestrator process is currently holding a row.
package instanceid
