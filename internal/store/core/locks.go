// -------------------------------------------------------------------------------
// Core Advisory Lock IDs
//
// Author: Alex Freidah
//
// Stable lock IDs for multi-instance coordination via the AdvisoryLocker
// role. Each background service acquires its lock before running to
// prevent concurrent execution across instances. The IDs are engine-
// agnostic; the Postgres engine maps them to pg_try_advisory_lock and
// the SQLite engine no-ops since single-instance deployments do not need
// cross-process coordination.
//
// IDs are arbitrary but must be unique and stable across releases - a
// rotating ID would break in-flight leader election during a rolling
// upgrade.
// -------------------------------------------------------------------------------

package core

// LockRebalancer and the other advisory lock IDs, one per background service.
// The value is the contract: it appears in operator runbooks and in
// pg_locks output, so an ID is never reassigned or reordered.
const (
	LockRebalancer       int64 = 1001 // periodic object distribution across backends
	LockReplicator       int64 = 1002 // background replica creation
	LockCleanupQueue     int64 = 1003 // failed deletion retry processing
	LockMultipartCleanup int64 = 1004 // stale multipart upload removal
	LockLifecycle        int64 = 1005 // object expiration rule evaluation
	LockDrain            int64 = 1006 // backend drain and object migration
	LockUsageFlush       int64 = 1007 // usage counter flush to the database
	LockOverReplication  int64 = 1008 // excess replica removal
	LockReconcile        int64 = 1009 // backend-vs-database consistency check
	LockScrubber         int64 = 1010 // background integrity verification
	LockPendingReaper    int64 = 1011 // abandoned PUT-intent resolution
)
