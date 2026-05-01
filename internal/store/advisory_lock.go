// -------------------------------------------------------------------------------
// Advisory Lock Operations
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package store

import (
	"context"
	"fmt"
	"time"
)

// Advisory lock IDs for multi-instance coordination via PostgreSQL.
// Each background service acquires its lock before running to prevent
// concurrent execution across instances. IDs are arbitrary but must be
// unique and stable across releases.
//
//   - LockRebalancer       (1001) periodic object distribution across backends
//   - LockReplicator       (1002) background replica creation
//   - LockCleanupQueue     (1003) failed deletion retry processing
//   - LockMultipartCleanup (1004) stale multipart upload removal
//   - LockLifecycle        (1005) object expiration rule evaluation
//   - LockDrain            (1006) backend drain and object migration
//   - LockUsageFlush       (1007) usage counter flush to PostgreSQL (Redis mode)
//   - LockOverReplication  (1008) excess replica removal
//   - LockReconcile        (1009) backend-vs-database consistency check
//   - LockScrubber         (1010) background integrity verification
//   - LockPendingReaper    (1011) abandoned PUT-intent resolution
const (
	LockRebalancer       int64 = 1001
	LockReplicator       int64 = 1002
	LockCleanupQueue     int64 = 1003
	LockMultipartCleanup int64 = 1004
	LockLifecycle        int64 = 1005
	LockDrain            int64 = 1006
	LockUsageFlush       int64 = 1007
	LockOverReplication  int64 = 1008
	LockReconcile        int64 = 1009
	LockScrubber         int64 = 1010
	LockPendingReaper    int64 = 1011
)

// WithAdvisoryLock acquires a PostgreSQL session-level advisory lock on a
// dedicated connection from the pool. If the lock is acquired, fn runs and
// the connection is released (which releases the lock). If another session
// holds the lock, returns (false, nil). On DB error, returns (false, err).
func (s *Store) WithAdvisoryLock(ctx context.Context, lockID int64, fn func(ctx context.Context) error) (bool, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to acquire connection for advisory lock: %w", err)
	}
	defer conn.Release()

	var acquired bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", lockID).Scan(&acquired); err != nil {
		return false, fmt.Errorf("failed to attempt advisory lock: %w", err)
	}

	if !acquired {
		return false, nil
	}

	// Ensure the lock is released even if fn panics. Use a detached
	// context so the unlock succeeds even if the caller's ctx is cancelled.
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", lockID)
	}()

	return true, fn(ctx)
}
