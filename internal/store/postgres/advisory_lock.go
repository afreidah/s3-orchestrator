// -------------------------------------------------------------------------------
// Advisory Lock Operations
//
// Author: Alex Freidah
//
// Implements WithAdvisoryLock against pg_try_advisory_lock /
// pg_advisory_unlock so the orchestrator's background workers
// (rebalancer, replicator, cleanup queue, lifecycle, usage flush) can
// elect a single leader across multiple instances. Each worker uses a
// stable lock id; if the try-lock fails, the worker skips this tick
// and lets the leader run instead. Connection-affinity rules of pgx
// require the lock acquire/release to run on the same connection,
// which is what this wrapper guarantees.
// -------------------------------------------------------------------------------

package postgres

import (
	"context"
	"fmt"
	"time"
)

// Advisory lock IDs live in internal/store/core. The postgres engine
// only implements WithAdvisoryLock; callers reference the constants
// from core directly so no engine-specific symbol leaks into request-
// path code.

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
