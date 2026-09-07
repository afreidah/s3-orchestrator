// -------------------------------------------------------------------------------
// Postgres CountObjectsByPrefix - Integration Tests
//
// Author: Alex Freidah
//
// Cross-engine parity for the count that tells a bucket deletion whether the
// namespace it is about to drop still addresses anything. The same scenarios
// the SQLite unit tests cover run here against real Postgres, so a replicated
// object counts once on both engines and a name carrying a LIKE metacharacter
// counts its own keys.
// -------------------------------------------------------------------------------

//go:build integration

package postgres

import (
	"context"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// recordForCount writes one copy and removes it when the test ends.
func recordForCount(t *testing.T, s *Store, key, backend string) {
	t.Helper()
	ctx := context.Background()
	req := &core.RecordObjectRequest{Key: key, Copies: []core.ObjectCopy{{Backend: backend}}, Size: 10}
	if _, _, err := s.RecordObject(ctx, req); err != nil {
		t.Fatalf("RecordObject(%s, %s): %v", key, backend, err)
	}
	t.Cleanup(func() { _, _, _ = s.DeleteObject(ctx, key) })
}

// TestStoreInt_CountObjectsByPrefix_CountsKeysNotCopies verifies a replicated
// object counts once, matching the SQLite answer.
func TestStoreInt_CountObjectsByPrefix_CountsKeysNotCopies(t *testing.T) {
	s := adapterPgStore(t)
	prefix := uniqueKey(t, "")

	recordForCount(t, s, prefix+"a", "backend-a")
	recordForCount(t, s, prefix+"b", "backend-a")
	recordForCount(t, s, prefix+"b", "backend-b")

	n, err := s.CountObjectsByPrefix(context.Background(), prefix)
	if err != nil {
		t.Fatalf("CountObjectsByPrefix: %v", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2", n)
	}
}

// TestStoreInt_CountObjectsByPrefix_EmptyIsZero verifies a prefix nothing lives
// under counts zero, which is what lets a bucket that was never written to be
// deleted.
func TestStoreInt_CountObjectsByPrefix_EmptyIsZero(t *testing.T) {
	s := adapterPgStore(t)

	n, err := s.CountObjectsByPrefix(context.Background(), uniqueKey(t, ""))
	if err != nil {
		t.Fatalf("CountObjectsByPrefix: %v", err)
	}
	if n != 0 {
		t.Errorf("count = %d, want 0", n)
	}
}

// TestStoreInt_CountObjectsByPrefix_EscapesMetacharacters verifies a bucket name
// carrying an underscore counts its own keys rather than every name the LIKE
// wildcard would also match.
func TestStoreInt_CountObjectsByPrefix_EscapesMetacharacters(t *testing.T) {
	s := adapterPgStore(t)
	prefix := uniqueKey(t, "")

	recordForCount(t, s, prefix+"my_bucket/a", "backend-a")
	recordForCount(t, s, prefix+"myXbucket/a", "backend-a")

	n, err := s.CountObjectsByPrefix(context.Background(), prefix+"my_bucket/")
	if err != nil {
		t.Fatalf("CountObjectsByPrefix: %v", err)
	}
	if n != 1 {
		t.Errorf("count = %d, want 1: the underscore matched another bucket", n)
	}
}
