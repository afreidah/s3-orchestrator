// -------------------------------------------------------------------------------
// SQLite Cleanup DLQ Tests
//
// Author: Alex Freidah
//
// Covers ListCleanupDLQ and RequeueCleanupDLQ: the operator listing scoped by
// backend, and the requeue that moves dead-lettered rows back into
// cleanup_queue so the worker retries them.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"testing"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// dlqAll moves every currently-pending cleanup row into the DLQ, stamping
// lastError, so a test can seed the dead-letter table from enqueued rows.
func dlqAll(t *testing.T, s *Store, lastError string) {
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

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestSqlite_ListCleanupDLQ_ScopeAndFields asserts the listing filters by
// backend and surfaces last_error and both timestamps.
func TestSqlite_ListCleanupDLQ_ScopeAndFields(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustEnqueueCleanup(t, s, "backend-a", "k1")
	mustEnqueueCleanup(t, s, "backend-a", "k2")
	mustEnqueueCleanup(t, s, "backend-b", "k3")
	dlqAll(t, s, "backend unavailable")

	if depth, _ := s.CleanupDLQDepth(ctx); depth != 3 {
		t.Fatalf("dlq depth = %d, want 3", depth)
	}

	all, err := s.ListCleanupDLQ(ctx, "", 10)
	if err != nil {
		t.Fatalf("ListCleanupDLQ(all): %v", err)
	}
	if len(all) != 3 {
		t.Errorf("listed %d, want 3", len(all))
	}

	scoped, err := s.ListCleanupDLQ(ctx, "backend-a", 10)
	if err != nil {
		t.Fatalf("ListCleanupDLQ(backend-a): %v", err)
	}
	if len(scoped) != 2 {
		t.Fatalf("scoped listed %d, want 2", len(scoped))
	}
	for i := range scoped {
		if scoped[i].BackendName != "backend-a" {
			t.Errorf("row %d backend = %q, want backend-a", i, scoped[i].BackendName)
		}
		if scoped[i].LastError != "backend unavailable" {
			t.Errorf("row %d last_error = %q", i, scoped[i].LastError)
		}
		if scoped[i].MovedAt.IsZero() || scoped[i].FirstEnqueued.IsZero() {
			t.Errorf("row %d has zero timestamps: %+v", i, scoped[i])
		}
	}
}

// TestSqlite_RequeueCleanupDLQ_MovesRowsBack asserts a scoped requeue returns
// the moved rows to cleanup_queue with fresh attempts, leaving other backends
// in the DLQ, and that an all-backends requeue drains the rest.
func TestSqlite_RequeueCleanupDLQ_MovesRowsBack(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustEnqueueCleanup(t, s, "backend-a", "k1")
	mustEnqueueCleanup(t, s, "backend-a", "k2")
	mustEnqueueCleanup(t, s, "backend-b", "k3")
	dlqAll(t, s, "backend unavailable")

	// Requeue just backend-a: 2 rows return to the queue, 1 stays in the DLQ.
	n, err := s.RequeueCleanupDLQ(ctx, "backend-a")
	if err != nil {
		t.Fatalf("RequeueCleanupDLQ(backend-a): %v", err)
	}
	if n != 2 {
		t.Errorf("requeued %d, want 2", n)
	}
	if depth, _ := s.CleanupDLQDepth(ctx); depth != 1 {
		t.Errorf("dlq depth after scoped requeue = %d, want 1", depth)
	}
	pending, err := s.GetPendingCleanups(ctx, 10)
	if err != nil {
		t.Fatalf("GetPendingCleanups: %v", err)
	}
	if len(pending) != 2 {
		t.Errorf("requeued rows in cleanup_queue = %d, want 2", len(pending))
	}
	for i := range pending {
		if pending[i].Attempts != 0 {
			t.Errorf("requeued row %d attempts = %d, want 0 (fresh)", i, pending[i].Attempts)
		}
	}

	// Requeue the rest (empty backend = all): the remaining backend-b row.
	n, err = s.RequeueCleanupDLQ(ctx, "")
	if err != nil {
		t.Fatalf("RequeueCleanupDLQ(all): %v", err)
	}
	if n != 1 {
		t.Errorf("requeued %d, want 1", n)
	}
	if depth, _ := s.CleanupDLQDepth(ctx); depth != 0 {
		t.Errorf("dlq depth after full requeue = %d, want 0", depth)
	}
}
