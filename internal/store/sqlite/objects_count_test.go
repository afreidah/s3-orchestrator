// -------------------------------------------------------------------------------
// SQLite Store - Prefix Count Tests
//
// Author: Alex Freidah
//
// The count that tells a bucket deletion whether the namespace it is about to
// drop still addresses anything. Keys are counted, not copies, and a name
// carrying a LIKE metacharacter counts its own.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"testing"
)

// TestSqlite_CountObjectsByPrefix_CountsKeysNotCopies verifies a replicated
// object counts once: an operator asking whether a bucket is empty is asking
// about keys, not about how many times each one is stored.
func TestSqlite_CountObjectsByPrefix_CountsKeysNotCopies(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "photos/a", "backend-a", 100)
	mustRecordObject(t, s, "photos/b", "backend-a", 100)
	mustRecordObject(t, s, "photos/b", "backend-b", 100)
	mustRecordObject(t, s, "other/c", "backend-a", 100)

	n, err := s.CountObjectsByPrefix(ctx, "photos/")
	if err != nil {
		t.Fatalf("CountObjectsByPrefix: %v", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2", n)
	}
}

// TestSqlite_CountObjectsByPrefix_EmptyIsZero verifies a prefix nothing lives
// under counts zero rather than erroring, which is what lets a bucket that was
// never written to be deleted.
func TestSqlite_CountObjectsByPrefix_EmptyIsZero(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	n, err := s.CountObjectsByPrefix(context.Background(), "empty/")
	if err != nil {
		t.Fatalf("CountObjectsByPrefix: %v", err)
	}
	if n != 0 {
		t.Errorf("count = %d, want 0", n)
	}
}

// TestSqlite_CountObjectsByPrefix_EscapesMetacharacters verifies a bucket whose
// name carries an underscore counts its own keys rather than every name the
// LIKE wildcard would also match.
func TestSqlite_CountObjectsByPrefix_EscapesMetacharacters(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	mustRecordObject(t, s, "my_bucket/a", "backend-a", 100)
	mustRecordObject(t, s, "myXbucket/a", "backend-a", 100)

	n, err := s.CountObjectsByPrefix(context.Background(), "my_bucket/")
	if err != nil {
		t.Fatalf("CountObjectsByPrefix: %v", err)
	}
	if n != 1 {
		t.Errorf("count = %d, want 1: the underscore matched another bucket", n)
	}
}

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
