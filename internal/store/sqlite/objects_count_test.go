// -------------------------------------------------------------------------------
// SQLite Store - Prefix Count Tests
//
// Author: Alex Freidah
//
// The count that tells a bucket deletion whether the namespace it is about to
// drop still addresses anything. What it counts is held to the shared listing
// check; this covers the failure only a SQLite handle can stage.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"testing"
)

// TestSqlite_CountObjectsByPrefix_ClosedStore verifies a store that cannot be
// queried reports the failure rather than answering zero, which would let a
// bucket holding objects be deleted.
func TestSqlite_CountObjectsByPrefix_ClosedStore(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if err := s.db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := s.CountObjectsByPrefix(context.Background(), "photos/"); err == nil {
		t.Fatal("a closed store answered a count")
	}
}
