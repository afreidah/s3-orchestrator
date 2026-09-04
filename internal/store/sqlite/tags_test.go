// -------------------------------------------------------------------------------
// Object Tag Tests
//
// Author: Alex Freidah
//
// Drives the real store rather than a mock, because what these guard against is
// SQL: a migration that never ran, an ordering the query does not actually
// impose, and the clear-on-write and cascade rules that keep a key's tags tied
// to the object currently stored under it rather than to the path.
//
// The same sequences run against Postgres in the integration suite; a
// divergence between the two engines is the failure mode #1229 and #1230 were.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// tagNames flattens a tag set to its keys, in the order the store returned
// them, so ordering assertions read as a list rather than a loop.
func tagNames(tags []core.Tag) []string {
	out := make([]string, len(tags))
	for i, tg := range tags {
		out[i] = tg.Key
	}
	return out
}

// seedObject records one copy of an object so the tag paths have something to
// hang off.
func seedObject(t *testing.T, s *Store, key, backend string) {
	t.Helper()
	if _, _, err := s.RecordObject(context.Background(), &core.RecordObjectRequest{Key: key, Backend: backend, Size: 1024}); err != nil {
		t.Fatalf("RecordObject(%s, %s): %v", key, backend, err)
	}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestObjectTags_ReplaceAndRead covers the round trip and the ordering the
// Tagging XML response depends on: the query sorts by key, so a set written in
// reverse order reads back sorted rather than in insertion order.
func TestObjectTags_ReplaceAndRead(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	seedObject(t, s, "bucket/tagged", "backend-a")

	tags := []core.Tag{{Key: "zeta", Value: "3"}, {Key: "alpha", Value: "1"}, {Key: "mid", Value: "2"}}
	if err := s.ReplaceObjectTags(ctx, "bucket/tagged", tags); err != nil {
		t.Fatalf("ReplaceObjectTags: %v", err)
	}

	got, err := s.GetObjectTags(ctx, "bucket/tagged")
	if err != nil {
		t.Fatalf("GetObjectTags: %v", err)
	}
	want := []string{"alpha", "mid", "zeta"}
	if names := tagNames(got); len(names) != 3 || names[0] != want[0] || names[1] != want[1] || names[2] != want[2] {
		t.Errorf("tag order = %v, want %v", names, want)
	}
	for _, tg := range got {
		if tg.Key == "alpha" && tg.Value != "1" {
			t.Errorf("alpha value = %q, want %q", tg.Value, "1")
		}
	}
}

// TestCountObjectTags_MatchesTheSetSize verifies the count the read path serves
// agrees with the set the tagging endpoint returns. A count that drifts from
// the set sends clients after tags that are not there, or hides ones that are.
func TestCountObjectTags_MatchesTheSetSize(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	seedObject(t, s, "bucket/tagged", "backend-a")

	tags := []core.Tag{{Key: "a", Value: "1"}, {Key: "b", Value: "2"}, {Key: "c", Value: "3"}}
	if err := s.ReplaceObjectTags(ctx, "bucket/tagged", tags); err != nil {
		t.Fatalf("ReplaceObjectTags: %v", err)
	}

	n, err := s.CountObjectTags(ctx, "bucket/tagged")
	if err != nil {
		t.Fatalf("CountObjectTags: %v", err)
	}
	if n != len(tags) {
		t.Errorf("count = %d, want %d", n, len(tags))
	}
}

// TestCountObjectTags_FollowsAReplace verifies the count tracks a set that
// shrinks. Replace is a delete plus inserts, so a count reading stale rows
// would keep reporting the size the object used to have.
func TestCountObjectTags_FollowsAReplace(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	seedObject(t, s, "bucket/tagged", "backend-a")

	if err := s.ReplaceObjectTags(ctx, "bucket/tagged", []core.Tag{
		{Key: "a", Value: "1"}, {Key: "b", Value: "2"}, {Key: "c", Value: "3"},
	}); err != nil {
		t.Fatalf("ReplaceObjectTags: %v", err)
	}
	if err := s.ReplaceObjectTags(ctx, "bucket/tagged", []core.Tag{{Key: "a", Value: "1"}}); err != nil {
		t.Fatalf("ReplaceObjectTags (shrink): %v", err)
	}

	n, err := s.CountObjectTags(ctx, "bucket/tagged")
	if err != nil {
		t.Fatalf("CountObjectTags: %v", err)
	}
	if n != 1 {
		t.Errorf("count = %d, want 1", n)
	}
}

// TestCountObjectTags_UntaggedObject verifies an object holding no tags counts
// zero rather than erroring. The read path asks this of every object it
// serves, and most of them carry no tags at all.
func TestCountObjectTags_UntaggedObject(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seedObject(t, s, "bucket/plain", "backend-a")

	n, err := s.CountObjectTags(context.Background(), "bucket/plain")
	if err != nil {
		t.Fatalf("CountObjectTags: %v", err)
	}
	if n != 0 {
		t.Errorf("count = %d, want 0", n)
	}
}

// TestCountObjectTags_UnknownKey verifies a key the store has never held counts
// zero too. The read path reaches the count only after the object has been
// located, so a key with no rows is not a condition to report here.
func TestCountObjectTags_UnknownKey(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	n, err := s.CountObjectTags(context.Background(), "bucket/never-existed")
	if err != nil {
		t.Fatalf("CountObjectTags: %v", err)
	}
	if n != 0 {
		t.Errorf("count = %d, want 0", n)
	}
}

// TestMultipartTags_SurviveCreateToComplete verifies the set supplied at
// CreateMultipartUpload is held on the upload row and read back intact, which
// is what lets CompleteMultipartUpload apply it to the assembled object hours
// and many parts later.
func TestMultipartTags_SurviveCreateToComplete(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	tags := []core.Tag{{Key: "zeta", Value: "3"}, {Key: "alpha", Value: "1"}}
	if err := s.CreateMultipartUpload(ctx, &core.CreateMultipartUploadParams{
		UploadID:    "upload-1",
		ObjectKey:   "bucket/big",
		BackendName: "backend-a",
		Tags:        tags,
	}); err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}

	mu, err := s.GetMultipartUpload(ctx, "upload-1")
	if err != nil {
		t.Fatalf("GetMultipartUpload: %v", err)
	}
	if names := tagNames(mu.Tags); len(names) != 2 || names[0] != "alpha" || names[1] != "zeta" {
		t.Errorf("tags = %v, want [alpha zeta]", names)
	}
	for _, tg := range mu.Tags {
		if tg.Key == "zeta" && tg.Value != "3" {
			t.Errorf("zeta value = %q, want 3", tg.Value)
		}
	}
}

// TestMultipartTags_UntaggedUploadReadsEmpty verifies an upload created with no
// tags reads back with none rather than failing to decode an empty column.
func TestMultipartTags_UntaggedUploadReadsEmpty(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.CreateMultipartUpload(ctx, &core.CreateMultipartUploadParams{
		UploadID:    "upload-2",
		ObjectKey:   "bucket/plain",
		BackendName: "backend-a",
	}); err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}

	mu, err := s.GetMultipartUpload(ctx, "upload-2")
	if err != nil {
		t.Fatalf("GetMultipartUpload: %v", err)
	}
	if len(mu.Tags) != 0 {
		t.Errorf("expected no tags, got %v", tagNames(mu.Tags))
	}
}

// TestObjectTags_DuplicateKeyInsertFails verifies the primary key is real: a
// set naming the same key twice reaches the insert and conflicts rather than
// silently collapsing to one row. Core rejects this before it gets here, so
// this pins the storage-level guard behind that.
func TestObjectTags_DuplicateKeyInsertFails(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	seedObject(t, s, "bucket/dup", "backend-a")

	err := s.WithTx(ctx, func(ctx context.Context, tx core.TxAdapter) error {
		if err := tx.InsertObjectTag(ctx, "bucket/dup", "k", "1"); err != nil {
			return err
		}
		return tx.InsertObjectTag(ctx, "bucket/dup", "k", "2")
	})
	if err == nil {
		t.Fatal("expected the second insert of the same key to conflict")
	}
}

// TestObjectTags_ReadErrorOnClosedStore verifies a query failure surfaces
// rather than reading as an empty tag set, which would make a broken database
// indistinguishable from an untagged object.
func TestObjectTags_ReadErrorOnClosedStore(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	seedObject(t, s, "bucket/closed", "backend-a")
	s.Close()

	if _, err := s.GetObjectTags(ctx, "bucket/closed"); err == nil {
		t.Error("expected an error reading tags from a closed store, got nil")
	}
}

// TestObjectTags_BatchClearEmptyList verifies the batch clear short-circuits
// on an empty list rather than issuing a statement that matches nothing.
func TestObjectTags_BatchClearEmptyList(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.WithTx(ctx, func(ctx context.Context, tx core.TxAdapter) error {
		return tx.DeleteObjectTagsForKeys(ctx, nil)
	}); err != nil {
		t.Errorf("empty batch clear should be a no-op, got %v", err)
	}
}

// TestObjectTags_ReplaceIsNotAMerge verifies replace swaps the whole set. A
// read-modify-write would leave the first set's keys behind.
func TestObjectTags_ReplaceIsNotAMerge(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	seedObject(t, s, "bucket/swapped", "backend-a")

	if err := s.ReplaceObjectTags(ctx, "bucket/swapped", []core.Tag{{Key: "first", Value: "1"}}); err != nil {
		t.Fatalf("ReplaceObjectTags (first): %v", err)
	}
	if err := s.ReplaceObjectTags(ctx, "bucket/swapped", []core.Tag{{Key: "second", Value: "2"}}); err != nil {
		t.Fatalf("ReplaceObjectTags (second): %v", err)
	}

	got, err := s.GetObjectTags(ctx, "bucket/swapped")
	if err != nil {
		t.Fatalf("GetObjectTags: %v", err)
	}
	if names := tagNames(got); len(names) != 1 || names[0] != "second" {
		t.Errorf("tags after replace = %v, want [second]", names)
	}
}

// TestObjectTags_UntaggedObjectReadsEmpty verifies an object with no tags
// yields an empty set rather than an error, which is what lets the endpoint
// answer 200 with an empty TagSet instead of 404.
func TestObjectTags_UntaggedObjectReadsEmpty(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	seedObject(t, s, "bucket/bare", "backend-a")

	got, err := s.GetObjectTags(ctx, "bucket/bare")
	if err != nil {
		t.Fatalf("GetObjectTags: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no tags, got %v", tagNames(got))
	}
}

// TestObjectTags_DeleteRemovesSet covers the explicit delete, and the AWS rule
// that deleting an already-empty set succeeds rather than erroring:
// DeleteObjectTagging against an untagged object returns 204.
func TestObjectTags_DeleteRemovesSet(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	seedObject(t, s, "bucket/cleared", "backend-a")

	if err := s.ReplaceObjectTags(ctx, "bucket/cleared", []core.Tag{{Key: "k", Value: "v"}}); err != nil {
		t.Fatalf("ReplaceObjectTags: %v", err)
	}
	if err := s.DeleteObjectTags(ctx, "bucket/cleared"); err != nil {
		t.Fatalf("DeleteObjectTags: %v", err)
	}

	got, err := s.GetObjectTags(ctx, "bucket/cleared")
	if err != nil {
		t.Fatalf("GetObjectTags: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected the set removed, got %v", tagNames(got))
	}

	if err := s.DeleteObjectTags(ctx, "bucket/cleared"); err != nil {
		t.Errorf("deleting an already-empty set should be a no-op, got %v", err)
	}
}

// TestObjectTags_OverwriteClearsSet pins the AWS rule that motivates
// clear-on-write: a PUT is a full replacement, so the object landing at a key
// starts with no tags rather than inheriting the previous occupant's.
func TestObjectTags_OverwriteClearsSet(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	seedObject(t, s, "bucket/overwritten", "backend-a")

	if err := s.ReplaceObjectTags(ctx, "bucket/overwritten", []core.Tag{{Key: "old", Value: "1"}}); err != nil {
		t.Fatalf("ReplaceObjectTags: %v", err)
	}
	seedObject(t, s, "bucket/overwritten", "backend-a")

	got, err := s.GetObjectTags(ctx, "bucket/overwritten")
	if err != nil {
		t.Fatalf("GetObjectTags: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("overwrite left the previous object's tags behind: %v", tagNames(got))
	}
}

// TestObjectTags_DeleteObjectCascades verifies removing the object takes its
// tags with it, so nothing is left for a later object at the same key.
func TestObjectTags_DeleteObjectCascades(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	seedObject(t, s, "bucket/doomed", "backend-a")

	if err := s.ReplaceObjectTags(ctx, "bucket/doomed", []core.Tag{{Key: "k", Value: "v"}}); err != nil {
		t.Fatalf("ReplaceObjectTags: %v", err)
	}
	if _, _, err := s.DeleteObject(ctx, "bucket/doomed"); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}

	got, err := s.GetObjectTags(ctx, "bucket/doomed")
	if err != nil {
		t.Fatalf("GetObjectTags: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("tags survived the object: %v", tagNames(got))
	}
}

// TestObjectTags_BatchDeleteCascades covers the delete-prefix path, which
// removes many keys in one statement and has to clear all of their tags.
func TestObjectTags_BatchDeleteCascades(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	keys := []string{"bucket/b1", "bucket/b2"}
	for _, k := range keys {
		seedObject(t, s, k, "backend-a")
		if err := s.ReplaceObjectTags(ctx, k, []core.Tag{{Key: "k", Value: "v"}}); err != nil {
			t.Fatalf("ReplaceObjectTags(%s): %v", k, err)
		}
	}

	if _, _, err := s.DeleteObjectsBatch(ctx, keys); err != nil {
		t.Fatalf("DeleteObjectsBatch: %v", err)
	}

	for _, k := range keys {
		got, err := s.GetObjectTags(ctx, k)
		if err != nil {
			t.Fatalf("GetObjectTags(%s): %v", k, err)
		}
		if len(got) != 0 {
			t.Errorf("tags survived batch delete of %s: %v", k, tagNames(got))
		}
	}
}

// TestObjectTags_ReplicaRemovalKeepsTags is the case that makes the cascade
// conditional. Tags belong to the object, so dropping one replica of a
// two-copy object must leave them alone; clearing here would be silent data
// loss on an object that still exists.
func TestObjectTags_ReplicaRemovalKeepsTags(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	seedObject(t, s, "bucket/replicated", "backend-a")
	if _, err := s.MoveObjectLocation(ctx, "bucket/replicated", "backend-a", "backend-b"); err != nil {
		t.Fatalf("MoveObjectLocation: %v", err)
	}
	seedObject(t, s, "bucket/replicated", "backend-a")
	if err := s.ReplaceObjectTags(ctx, "bucket/replicated", []core.Tag{{Key: "keep", Value: "me"}}); err != nil {
		t.Fatalf("ReplaceObjectTags: %v", err)
	}

	if _, err := s.DeleteObjectLocation(ctx, "bucket/replicated", "backend-b"); err != nil {
		t.Fatalf("DeleteObjectLocation: %v", err)
	}

	got, err := s.GetObjectTags(ctx, "bucket/replicated")
	if err != nil {
		t.Fatalf("GetObjectTags: %v", err)
	}
	if names := tagNames(got); len(names) != 1 || names[0] != "keep" {
		t.Errorf("removing one replica cleared the object's tags: %v", names)
	}

	// Removing what is now the last copy takes them.
	if _, err := s.DeleteObjectLocation(ctx, "bucket/replicated", "backend-a"); err != nil {
		t.Fatalf("DeleteObjectLocation (last): %v", err)
	}
	got, err = s.GetObjectTags(ctx, "bucket/replicated")
	if err != nil {
		t.Fatalf("GetObjectTags after last copy: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("tags survived the last copy: %v", tagNames(got))
	}
}
