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

// Advisory lock IDs assigned to each background service.
//
//   - LockRebalancer       (1001) periodic object distribution across backends
//   - LockReplicator       (1002) background replica creation
//   - LockCleanupQueue     (1003) failed deletion retry processing
//   - LockMultipartCleanup (1004) stale multipart upload removal
//   - LockLifecycle        (1005) object expiration rule evaluation
//   - LockDrain            (1006) backend drain and object migration
//   - LockUsageFlush       (1007) usage counter flush to the database
//   - LockOverReplication  (1008) excess replica removal
//   - LockReconcile        (1009) backend-vs-database consistency check
//   - LockScrubber         (1010) background integrity verification
//   - LockPendingReaper    (1011) abandoned PUT-intent resolution
//   - LockMultipartDEKBackfill (1012) legacy multipart-upload DEK migration
const (
	// LockRebalancer is held by the periodic rebalancer.
	LockRebalancer int64 = 1001
	// LockReplicator is held by the background replicator.
	LockReplicator int64 = 1002
	// LockCleanupQueue is held by the cleanup-queue retry worker.
	LockCleanupQueue int64 = 1003
	// LockMultipartCleanup is held by the stale-multipart sweeper.
	LockMultipartCleanup int64 = 1004
	// LockLifecycle is held by the lifecycle expiration worker.
	LockLifecycle int64 = 1005
	// LockDrain is held by an in-progress backend drain.
	LockDrain int64 = 1006
	// LockUsageFlush is held by the usage-counter flush worker.
	LockUsageFlush int64 = 1007
	// LockOverReplication is held by the excess-replica cleaner.
	LockOverReplication int64 = 1008
	// LockReconcile is held by the reconciler.
	LockReconcile int64 = 1009
	// LockScrubber is held by the integrity scrubber.
	LockScrubber int64 = 1010
	// LockPendingReaper is held by the pending-intent reaper.
	LockPendingReaper int64 = 1011
	// LockMultipartDEKBackfill is held by the periodic legacy
	// multipart-upload DEK migration worker. Per-uploadID locks acquired
	// inside the worker live in a separate high-bit namespace so they
	// never collide with this service-level ID.
	LockMultipartDEKBackfill int64 = 1012
)
