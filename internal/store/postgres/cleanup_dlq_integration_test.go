// -------------------------------------------------------------------------------
// Cleanup DLQ Integration Tests
//
// Author: Alex Freidah
//
// Exercises ListCleanupDLQ (backend scoping + field mapping) and the
// writable-CTE RequeueCleanupDLQ against a real Postgres container. The
// atomic DELETE ... RETURNING -> INSERT move is not covered by unit tests.
// -------------------------------------------------------------------------------

//go:build integration

package postgres

import (
	"context"
	"math"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// dlqAllPg moves every pending cleanup row into the DLQ, stamping lastError.
func dlqAllPg(t *testing.T, s *Store, lastError string) {
	t.Helper()
	ctx := context.Background()
	pending, err := s.GetPendingCleanups(ctx, 100)
	if err != nil {
		t.Fatalf("GetPendingCleanups: %v", err)
	}
	for i := range pending {
		if _, err := s.MoveCleanupToDLQ(ctx, pending[i].ID, lastError); err != nil {
			t.Fatalf("MoveCleanupToDLQ: %v", err)
		}
	}
}

// resetCleanupTablesPg empties cleanup_queue and cleanup_dlq so a test that
// asserts an exact row count measures only what it enqueued itself.
//
// dlqAllPg sweeps every pending row in the database, not just this test's, and
// EnqueueCleanup dates next_retry forward: a row an earlier test left behind
// becomes pending once its backoff elapses, so whether it lands in the DLQ
// depends on how long the suite took to reach here. Clearing first makes the
// count depend on the test rather than on what ran before it.
func resetCleanupTablesPg(t *testing.T, s *Store) {
	t.Helper()
	for _, table := range []string{"cleanup_queue", "cleanup_dlq"} {
		if _, err := s.pool.Exec(context.Background(), "DELETE FROM "+table); err != nil {
			t.Fatalf("clear %s: %v", table, err)
		}
	}
}

// mustEnqueuePg enqueues one cleanup row under a fresh key, failing on error.
func mustEnqueuePg(t *testing.T, s *Store, backend string, size int64) {
	t.Helper()
	if err := s.EnqueueCleanup(context.Background(), backend, uniqueKey(t, "dlq"), "delete_failed", size); err != nil {
		t.Fatalf("EnqueueCleanup: %v", err)
	}
}

// assertBackendADLQRow asserts a dead-lettered row carries backend-a's fields
// and non-zero timestamps.
func assertBackendADLQRow(t *testing.T, it core.CleanupDLQItem) {
	t.Helper()
	if it.BackendName != "backend-a" {
		t.Errorf("backend = %q, want backend-a", it.BackendName)
	}
	if it.LastError != "backend unavailable" || it.Reason != "delete_failed" {
		t.Errorf("fields = %+v", it)
	}
	if it.MovedAt.IsZero() || it.FirstEnqueued.IsZero() {
		t.Errorf("zero timestamps: %+v", it)
	}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestStoreInt_ListCleanupDLQ_ScopeAndFields asserts the listing filters by
// backend and maps every column (reason, last_error, and both timestamps)
// through from a real cleanup_dlq row.
func TestStoreInt_ListCleanupDLQ_ScopeAndFields(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()

	resetCleanupTablesPg(t, s)
	mustEnqueuePg(t, s, "backend-a", 2048)
	mustEnqueuePg(t, s, "backend-a", 1024)
	mustEnqueuePg(t, s, "backend-b", 512)
	dlqAllPg(t, s, "backend unavailable")

	scoped, err := s.ListCleanupDLQ(ctx, "backend-a", 10)
	if err != nil {
		t.Fatalf("ListCleanupDLQ(backend-a): %v", err)
	}
	if len(scoped) != 2 {
		t.Fatalf("scoped listed %d, want 2", len(scoped))
	}
	for i := range scoped {
		assertBackendADLQRow(t, scoped[i])
	}

	all, err := s.ListCleanupDLQ(ctx, "", 10)
	if err != nil {
		t.Fatalf("ListCleanupDLQ(all): %v", err)
	}
	if len(all) < 3 {
		t.Errorf("listed %d, want >= 3", len(all))
	}
}

// TestStoreInt_RequeueCleanupDLQ_MovesRowsBack asserts the writable-CTE requeue
// moves a backend's dead-lettered rows back into cleanup_queue atomically:
// the DLQ depth drops by the returned count and the rows reappear pending with
// fresh attempts.
// The rows this test looks for are named and then found by name. Counting a
// backend's rows in a windowed read cannot work here: the queue is shared with
// every other test in the package, GetPendingCleanups takes a limit, and rows
// left by earlier tests sort ahead of these ones, so a run late enough in the
// package would look past them and read zero.
func TestStoreInt_RequeueCleanupDLQ_MovesRowsBack(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()

	// uniqueKey is derived from the test name, so every row here carries the
	// same key and the count has to be kept separately from the name.
	const rows = 3
	key := uniqueKey(t, "requeue")
	for range rows {
		if err := s.EnqueueCleanup(ctx, "backend-a", key, "delete_failed", 256); err != nil {
			t.Fatalf("EnqueueCleanup: %v", err)
		}
	}
	dlqAllPg(t, s, "backend unavailable")

	before, _ := s.CleanupDLQDepth(ctx)
	scopedBefore, _ := s.ListCleanupDLQ(ctx, "backend-a", 100)

	n, err := s.RequeueCleanupDLQ(ctx, "backend-a")
	if err != nil {
		t.Fatalf("RequeueCleanupDLQ(backend-a): %v", err)
	}
	if n != int64(len(scopedBefore)) {
		t.Errorf("requeued %d, want %d", n, len(scopedBefore))
	}

	after, _ := s.CleanupDLQDepth(ctx)
	if before-after != n {
		t.Errorf("dlq depth delta = %d, want %d", before-after, n)
	}
	// Requeued rows are immediately eligible with fresh attempts. The limit is
	// the whole queue rather than a page, so a row this test enqueued cannot be
	// missed by sorting behind rows it did not.
	pending, err := s.GetPendingCleanups(ctx, math.MaxInt32)
	if err != nil {
		t.Fatalf("GetPendingCleanups: %v", err)
	}
	found := 0
	for i := range pending {
		if pending[i].ObjectKey != key {
			continue
		}
		found++
		if pending[i].Attempts != 0 {
			t.Errorf("requeued row attempts = %d, want 0", pending[i].Attempts)
		}
	}
	if found != rows {
		t.Errorf("requeued rows visible in queue = %d, want %d", found, rows)
	}
}
