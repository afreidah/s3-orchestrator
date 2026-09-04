// -------------------------------------------------------------------------------
// Object Tag Integration Tests
//
// Author: Alex Freidah
//
// Drives the object_tags bindings against a real Postgres, running the same
// sequences the SQLite suite runs so a divergence between the two engines
// fails here rather than in production. Per-engine copies of shared logic are
// how the two drifted on quota arithmetic and key collation.
//
// The cases that matter are the conditional ones: removing one replica of a
// multi-copy object has to leave its tags alone, while removing the last copy
// or overwriting the object has to take them.
// -------------------------------------------------------------------------------

//go:build integration

package postgres

import (
	"context"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// tagNames flattens a tag set to its keys in the order the store returned
// them, so ordering assertions read as a list rather than a loop.
func tagNames(tags []core.Tag) []string {
	out := make([]string, len(tags))
	for i, tg := range tags {
		out[i] = tg.Key
	}
	return out
}

// seedTaggedObject records one copy of an object and gives it a tag set.
func seedTaggedObject(t *testing.T, s *Store, key, backend string, tags []core.Tag) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Backend: backend, Size: 1024}); err != nil {
		t.Fatalf("RecordObject(%s, %s): %v", key, backend, err)
	}
	if tags != nil {
		if err := s.ReplaceObjectTags(ctx, key, tags); err != nil {
			t.Fatalf("ReplaceObjectTags(%s): %v", key, err)
		}
	}
}

// assertNoTags fails when the key still carries any tag.
func assertNoTags(t *testing.T, s *Store, key, msg string) {
	t.Helper()
	got, err := s.GetObjectTags(context.Background(), key)
	if err != nil {
		t.Fatalf("GetObjectTags(%s): %v", key, err)
	}
	if len(got) != 0 {
		t.Errorf("%s: %v", msg, tagNames(got))
	}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestStoreInt_CountObjectTags_MatchesTheSetSize verifies the count the read
// path serves agrees with the set the tagging endpoint returns, and that it
// follows the set down when a replace shrinks it. A count drifting from the
// set is what sends clients after tags that are not there.
func TestStoreInt_CountObjectTags_MatchesTheSetSize(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "tags-count")

	seedTaggedObject(t, s, key, "backend-a", []core.Tag{
		{Key: "a", Value: "1"},
		{Key: "b", Value: "2"},
		{Key: "c", Value: "3"},
	})

	n, err := s.CountObjectTags(ctx, key)
	if err != nil {
		t.Fatalf("CountObjectTags: %v", err)
	}
	if n != 3 {
		t.Fatalf("count = %d, want 3", n)
	}

	if err := s.ReplaceObjectTags(ctx, key, []core.Tag{{Key: "a", Value: "1"}}); err != nil {
		t.Fatalf("ReplaceObjectTags (shrink): %v", err)
	}
	if n, err = s.CountObjectTags(ctx, key); err != nil {
		t.Fatalf("CountObjectTags after shrink: %v", err)
	}
	if n != 1 {
		t.Errorf("count after shrink = %d, want 1", n)
	}
}

// TestStoreInt_CountObjectTags_UntaggedAndUnknown verifies both an object
// holding no tags and a key the store has never held count zero rather than
// erroring. The read path asks this of every object it serves, and reaches it
// only once the object has been located.
func TestStoreInt_CountObjectTags_UntaggedAndUnknown(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "tags-count-untagged")

	seedTaggedObject(t, s, key, "backend-a", nil)

	n, err := s.CountObjectTags(ctx, key)
	if err != nil {
		t.Fatalf("CountObjectTags: %v", err)
	}
	if n != 0 {
		t.Errorf("untagged count = %d, want 0", n)
	}

	if n, err = s.CountObjectTags(ctx, uniqueKey(t, "tags-count-absent")); err != nil {
		t.Fatalf("CountObjectTags on an unknown key: %v", err)
	}
	if n != 0 {
		t.Errorf("unknown-key count = %d, want 0", n)
	}
}

// TestStoreInt_ObjectTags_ReplaceAndRead covers the round trip and the
// ordering the Tagging XML response depends on: the query sorts by key, so a
// set written in reverse reads back sorted rather than in insertion order.
func TestStoreInt_ObjectTags_ReplaceAndRead(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "tags-roundtrip")

	seedTaggedObject(t, s, key, "backend-a", []core.Tag{
		{Key: "zeta", Value: "3"},
		{Key: "alpha", Value: "1"},
		{Key: "mid", Value: "2"},
	})

	got, err := s.GetObjectTags(ctx, key)
	if err != nil {
		t.Fatalf("GetObjectTags: %v", err)
	}
	want := []string{"alpha", "mid", "zeta"}
	names := tagNames(got)
	if len(names) != len(want) {
		t.Fatalf("tag count = %d, want %d: %v", len(names), len(want), names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("tag order = %v, want %v", names, want)
		}
	}
}

// TestStoreInt_ObjectTags_DuplicateKeyInsertFails verifies the primary key is
// real: a set naming the same key twice reaches the insert and conflicts
// rather than silently collapsing to one row. Core rejects this before it gets
// here, so this pins the storage-level guard sitting behind that.
func TestStoreInt_ObjectTags_DuplicateKeyInsertFails(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "tags-dup")
	seedTaggedObject(t, s, key, "backend-a", nil)

	err := s.WithTx(ctx, func(ctx context.Context, tx core.TxAdapter) error {
		if err := tx.InsertObjectTag(ctx, key, "k", "1"); err != nil {
			return err
		}
		return tx.InsertObjectTag(ctx, key, "k", "2")
	})
	if err == nil {
		t.Fatal("expected the second insert of the same key to conflict")
	}
}

// TestStoreInt_ObjectTags_BatchClearEmptyList verifies the batch clear accepts
// an empty list without erroring, since the delete paths call it
// unconditionally.
func TestStoreInt_ObjectTags_BatchClearEmptyList(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()

	if err := s.WithTx(ctx, func(ctx context.Context, tx core.TxAdapter) error {
		return tx.DeleteObjectTagsForKeys(ctx, nil)
	}); err != nil {
		t.Errorf("empty batch clear should succeed, got %v", err)
	}
}

// TestStoreInt_ObjectTags_LookupIndexAnswersReverseQuery verifies the
// (tag_key, tag_value) index answers the reverse direction, which is what
// lifecycle-by-tag will depend on: finding the objects carrying a tag without
// scanning every row.
func TestStoreInt_ObjectTags_LookupIndexAnswersReverseQuery(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	hit := uniqueKey(t, "tags-idx-hit")
	miss := uniqueKey(t, "tags-idx-miss")

	seedTaggedObject(t, s, hit, "backend-a", []core.Tag{{Key: "retain", Value: "30d"}})
	seedTaggedObject(t, s, miss, "backend-a", []core.Tag{{Key: "retain", Value: "7d"}})

	var got string
	if err := s.pool.QueryRow(ctx,
		`SELECT object_key FROM object_tags WHERE tag_key = $1 AND tag_value = $2`,
		"retain", "30d",
	).Scan(&got); err != nil {
		t.Fatalf("reverse lookup: %v", err)
	}
	if got != hit {
		t.Errorf("reverse lookup returned %q, want %q", got, hit)
	}
}

// TestStoreInt_ObjectTags_ReplaceIsNotAMerge verifies replace swaps the whole
// set. A read-modify-write would leave the first set's keys behind.
func TestStoreInt_ObjectTags_ReplaceIsNotAMerge(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "tags-swap")

	seedTaggedObject(t, s, key, "backend-a", []core.Tag{{Key: "first", Value: "1"}})
	if err := s.ReplaceObjectTags(ctx, key, []core.Tag{{Key: "second", Value: "2"}}); err != nil {
		t.Fatalf("ReplaceObjectTags: %v", err)
	}

	got, err := s.GetObjectTags(ctx, key)
	if err != nil {
		t.Fatalf("GetObjectTags: %v", err)
	}
	if names := tagNames(got); len(names) != 1 || names[0] != "second" {
		t.Errorf("tags after replace = %v, want [second]", names)
	}
}

// TestStoreInt_ObjectTags_UntaggedReadsEmpty verifies an object with no tags
// yields an empty set rather than an error, which is what lets the endpoint
// answer 200 with an empty TagSet instead of 404.
func TestStoreInt_ObjectTags_UntaggedReadsEmpty(t *testing.T) {
	s := adapterPgStore(t)
	key := uniqueKey(t, "tags-bare")

	seedTaggedObject(t, s, key, "backend-a", nil)
	assertNoTags(t, s, key, "expected no tags on an untagged object")
}

// TestStoreInt_ObjectTags_DeleteRemovesSet covers the explicit delete and the
// AWS rule that deleting an already-empty set succeeds rather than erroring.
func TestStoreInt_ObjectTags_DeleteRemovesSet(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "tags-delete")

	seedTaggedObject(t, s, key, "backend-a", []core.Tag{{Key: "k", Value: "v"}})
	if err := s.DeleteObjectTags(ctx, key); err != nil {
		t.Fatalf("DeleteObjectTags: %v", err)
	}
	assertNoTags(t, s, key, "expected the set removed")

	if err := s.DeleteObjectTags(ctx, key); err != nil {
		t.Errorf("deleting an already-empty set should be a no-op, got %v", err)
	}
}

// TestStoreInt_ObjectTags_OverwriteClearsSet pins the AWS rule that motivates
// clear-on-write: a PUT is a full replacement, so the object landing at a key
// starts with no tags rather than inheriting the previous occupant's.
func TestStoreInt_ObjectTags_OverwriteClearsSet(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "tags-overwrite")

	seedTaggedObject(t, s, key, "backend-a", []core.Tag{{Key: "old", Value: "1"}})
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Backend: "backend-a", Size: 2048}); err != nil {
		t.Fatalf("RecordObject (overwrite): %v", err)
	}
	assertNoTags(t, s, key, "overwrite left the previous object's tags behind")
}

// TestStoreInt_ObjectTags_DeleteObjectCascades verifies removing the object
// takes its tags with it, so nothing is left for a later object at the key.
func TestStoreInt_ObjectTags_DeleteObjectCascades(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "tags-doomed")

	seedTaggedObject(t, s, key, "backend-a", []core.Tag{{Key: "k", Value: "v"}})
	if _, _, err := s.DeleteObject(ctx, key); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	assertNoTags(t, s, key, "tags survived the object")
}

// TestStoreInt_ObjectTags_BatchDeleteCascades covers the delete-prefix path,
// which removes many keys in one statement and clears all of their tags. The
// batch also takes its key locks in sorted order, which this exercises.
func TestStoreInt_ObjectTags_BatchDeleteCascades(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	keys := []string{uniqueKey(t, "batch-b"), uniqueKey(t, "batch-a")}

	for _, k := range keys {
		seedTaggedObject(t, s, k, "backend-a", []core.Tag{{Key: "k", Value: "v"}})
	}
	if _, _, err := s.DeleteObjectsBatch(ctx, keys); err != nil {
		t.Fatalf("DeleteObjectsBatch: %v", err)
	}
	for _, k := range keys {
		assertNoTags(t, s, k, "tags survived batch delete")
	}
}

// TestStoreInt_ObjectTags_ReplicaRemovalKeepsTags is the case that makes the
// cascade conditional. Tags belong to the object, so dropping one replica of a
// two-copy object must leave them alone; clearing here would be silent data
// loss on an object that still exists. Removing what is then the last copy
// has to take them.
func TestStoreInt_ObjectTags_ReplicaRemovalKeepsTags(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "tags-replicated")

	seedTaggedObject(t, s, key, "backend-a", nil)
	if _, err := s.MoveObjectLocation(ctx, key, "backend-a", "backend-b"); err != nil {
		t.Fatalf("MoveObjectLocation: %v", err)
	}
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Backend: "backend-a", Size: 1024}); err != nil {
		t.Fatalf("RecordObject (second copy): %v", err)
	}
	if err := s.ReplaceObjectTags(ctx, key, []core.Tag{{Key: "keep", Value: "me"}}); err != nil {
		t.Fatalf("ReplaceObjectTags: %v", err)
	}

	if _, err := s.DeleteObjectLocation(ctx, key, "backend-b"); err != nil {
		t.Fatalf("DeleteObjectLocation: %v", err)
	}
	got, err := s.GetObjectTags(ctx, key)
	if err != nil {
		t.Fatalf("GetObjectTags: %v", err)
	}
	if names := tagNames(got); len(names) != 1 || names[0] != "keep" {
		t.Errorf("removing one replica cleared the object's tags: %v", names)
	}

	if _, err := s.DeleteObjectLocation(ctx, key, "backend-a"); err != nil {
		t.Fatalf("DeleteObjectLocation (last): %v", err)
	}
	assertNoTags(t, s, key, "tags survived the last copy")
}
