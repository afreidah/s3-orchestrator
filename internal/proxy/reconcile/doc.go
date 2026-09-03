// Package reconcile drives the two ways a backend's real contents are folded
// back into the ledger: sync, which imports everything the backend holds, and
// reconcile, which diffs both sides and applies the difference in each
// direction.
//
// Alongside the sorted-merge engine and its key streams, it owns the wiring -
// resolving the backend's lister, accounting the list calls against its API
// quota, and turning a stale ledger row into a delete plus a cleanup-queue
// sweep.
package reconcile
