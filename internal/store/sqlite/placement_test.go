// -------------------------------------------------------------------------------
// SQLite - Copy Placement Tests
//
// Author: Alex Freidah
//
// Drives PlaceCopy against the real engine: a placement commits only while its
// intent is still held and the source is still the copy the caller read, a
// move carries every column of the source row and queues the source's bytes,
// and a refused placement leaves rows and quotas as they were while queueing
// the copied bytes and dropping the intent.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// placedKey is the object every placement test places; firstWritePath is
// where its first write lands, and movedPath and replicaPath are where a move
// and a replica write the copied bytes.
const (
	placedKey      = "bucket/placed"
	firstWritePath = placedKey + "!w1"
	movedPath      = placedKey + "!m"
	replicaPath    = placedKey + "!r"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// recordWrite records one write of placedKey on backend-a at storageKey,
// stored in every form column and carrying an etag named for its path.
func recordWrite(t *testing.T, s *Store, storageKey string, size int64) {
	t.Helper()
	if _, _, err := s.RecordObject(context.Background(), &core.RecordObjectRequest{
		Key: placedKey, Size: size, Form: fullyPopulatedForm(),
		Copies:   []core.ObjectCopy{{Backend: "backend-a", StorageKey: storageKey}},
		Identity: &core.ObjectIdentity{ETag: `"` + storageKey + `"`},
	}); err != nil {
		t.Fatalf("RecordObject(%s): %v", storageKey, err)
	}
}

// claimFor claims an intent for size bytes of placedKey at path on backend.
func claimFor(t *testing.T, s *Store, backend, path string, size int64) *core.PendingObject {
	t.Helper()
	return mustClaimCopyIntent(t, s, placedKey, backend, path, size, core.AcceptForWrite)
}

// place runs PlaceCopy of src to the intent's path under the test reasons.
func place(s *Store, src *core.ObjectLocation, intent *core.PendingObject, removeSource bool) (int64, []core.CleanupItem, error) {
	return s.PlaceCopy(context.Background(), &core.Placement{
		Source: src, Intent: intent, RemoveSource: removeSource, Reasons: testMoveReasons,
	})
}

// assertDeclined checks a placement failed with want and left the copy set
// and both backends' byte counts as they were before it.
func assertDeclined(t *testing.T, s *Store, err, want error, before []core.ObjectLocation, usedA, usedB int64) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("PlaceCopy = %v, want %v", err, want)
	}
	after, lerr := s.GetAllObjectLocations(context.Background(), placedKey)
	if lerr != nil && !errors.Is(lerr, core.ErrObjectNotFound) {
		t.Fatalf("GetAllObjectLocations: %v", lerr)
	}
	if len(after) != len(before) {
		t.Fatalf("copies = %+v, want the %d held before", after, len(before))
	}
	for i := range before {
		if after[i].BackendName != before[i].BackendName || after[i].StorageKey != before[i].StorageKey {
			t.Errorf("copy %d = %s/%s, want %s/%s", i, after[i].BackendName, after[i].StorageKey,
				before[i].BackendName, before[i].StorageKey)
		}
	}
	if got := quotaBytesUsed(t, s, "backend-a"); got != usedA {
		t.Errorf("backend-a bytes = %d, want %d", got, usedA)
	}
	if got := quotaBytesUsed(t, s, "backend-b"); got != usedB {
		t.Errorf("backend-b bytes = %d, want %d", got, usedB)
	}
}

// assertIntentQueued checks the placement dropped its intent and returned one
// cleanup row for the intent's bytes under reason, committed to the queue.
func assertIntentQueued(t *testing.T, s *Store, cleanup []core.CleanupItem, intent *core.PendingObject, reason string) {
	t.Helper()
	if n := queryPendingCount(t, s); n != 0 {
		t.Errorf("pending intents = %d, want the placement's dropped", n)
	}
	if len(cleanup) != 1 {
		t.Fatalf("cleanup = %+v, want one row for the intent's bytes", cleanup)
	}
	got := cleanup[0]
	if got.BackendName != intent.BackendName || got.StorageKey != intent.StorageKey ||
		got.SizeBytes != intent.SizeBytes || got.Reason != reason {
		t.Errorf("cleanup row = %+v, want %s/%s %d bytes under %q",
			got, intent.BackendName, intent.StorageKey, intent.SizeBytes, reason)
	}
	for _, q := range queuedCleanups(t, s) {
		if q.ID == got.ID {
			return
		}
	}
	t.Errorf("cleanup row %d is not in the queue", got.ID)
}

// -------------------------------------------------------------------------
// DECLINED PLACEMENTS
// -------------------------------------------------------------------------

// TestPlaceCopy_ReplacedSourceIsCopyChanged verifies a move whose source a
// newer write replaced between the read and the placement records nothing.
// The newer write cleared the move's intent, so the copied bytes are queued
// as a superseded intent's.
func TestPlaceCopy_ReplacedSourceIsCopyChanged(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	recordWrite(t, s, firstWritePath, 100)
	read := storetest.LocationOn(t, s, placedKey, "backend-a")
	intent := claimFor(t, s, "backend-b", movedPath, 100)
	recordWrite(t, s, placedKey+"!w2", 200)
	before, _ := s.GetAllObjectLocations(context.Background(), placedKey)

	_, cleanup, err := place(s, read, intent, true)
	assertDeclined(t, s, err, core.ErrCopyChanged, before, 200, 0)
	assertIntentQueued(t, s, cleanup, intent, core.CleanupReasonSupersededIntent)
}

// TestPlaceCopy_SourceGoneIsCopyChanged verifies a placement whose source row
// no longer exists records nothing and queues the copied bytes.
func TestPlaceCopy_SourceGoneIsCopyChanged(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	intent := claimFor(t, s, "backend-b", movedPath, 100)
	_, cleanup, err := place(s,
		&core.ObjectLocation{ObjectKey: placedKey, BackendName: "backend-a", StorageKey: firstWritePath},
		intent, false)
	assertDeclined(t, s, err, core.ErrCopyChanged, nil, 0, 0)
	assertIntentQueued(t, s, cleanup, intent, testMoveReasons.Orphan)
}

// TestPlaceCopy_TargetHoldingCopyIsRefused verifies a placement onto a backend
// that already holds a copy of the key records nothing and queues the copied
// bytes.
func TestPlaceCopy_TargetHoldingCopyIsRefused(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	recordWrite(t, s, firstWritePath, 100)
	mustRecordReplica(t, s, placedKey, "backend-b", "backend-a", 100)
	before, _ := s.GetAllObjectLocations(context.Background(), placedKey)

	intent := claimFor(t, s, "backend-b", movedPath, 100)
	_, cleanup, err := place(s, storetest.LocationOn(t, s, placedKey, "backend-a"), intent, true)
	assertDeclined(t, s, err, core.ErrTargetRefused, before, 100, 100)
	assertIntentQueued(t, s, cleanup, intent, testMoveReasons.Orphan)
}

// TestPlaceCopy_SupersededIntentIsCopyChanged verifies a placement whose
// intent a client write cleared mid-copy records nothing and queues the copied
// bytes as a superseded intent's.
func TestPlaceCopy_SupersededIntentIsCopyChanged(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	recordWrite(t, s, firstWritePath, 100)
	intent := claimFor(t, s, "backend-b", replicaPath, 100)
	if err := s.DeletePending(context.Background(), intent.IntentID); err != nil {
		t.Fatalf("DeletePending: %v", err)
	}
	before, _ := s.GetAllObjectLocations(context.Background(), placedKey)

	_, cleanup, err := place(s, storetest.LocationOn(t, s, placedKey, "backend-a"), intent, false)
	assertDeclined(t, s, err, core.ErrCopyChanged, before, 100, 0)
	assertIntentQueued(t, s, cleanup, intent, core.CleanupReasonSupersededIntent)
}

// TestPlaceCopy_ClaimOnFullTargetIsRefused verifies a target without room for
// the source's bytes refuses the claim the copy takes before writing, so no
// intent is recorded.
func TestPlaceCopy_ClaimOnFullTargetIsRefused(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if err := s.SyncQuotaLimits(context.Background(), []config.BackendConfig{{Name: "backend-b", QuotaBytes: 50}}); err != nil {
		t.Fatalf("SyncQuotaLimits: %v", err)
	}
	fits, err := s.InsertPendingIfFits(context.Background(), &core.PendingObject{
		IntentID: "intent-full", ObjectKey: placedKey, StorageKey: replicaPath,
		BackendName: "backend-b", SizeBytes: 100, Role: core.PendingRoleCompanion,
	}, core.AcceptForWrite)
	if err != nil || fits {
		t.Fatalf("InsertPendingIfFits on a full target = %v, %v, want refused", fits, err)
	}
	if n := queryPendingCount(t, s); n != 0 {
		t.Errorf("pending intents = %d, want 0", n)
	}
}

// -------------------------------------------------------------------------
// COMMITTED PLACEMENTS
// -------------------------------------------------------------------------

// TestPlaceCopy_MoveCarriesTheSourceRow verifies a move lands the source row
// on the target unchanged but for backend and path, removes the source row,
// queues the source's bytes, clears the intent, and moves the bytes between
// the backends' counts. Last-Modified surviving the move is the reason
// created_at is carried.
func TestPlaceCopy_MoveCarriesTheSourceRow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	recordWrite(t, s, firstWritePath, 100)
	mustPatchCopy(t, s, placedKey, "backend-a", &core.CopyPatch{Probe: &core.CompressionProbe{Size: 90, Level: "default"}})
	backdateCreatedAt(t, s, placedKey, "backend-a", 24*time.Hour)
	src := storetest.LocationOn(t, s, placedKey, "backend-a")

	moved, cleanup, err := place(s, src, claimFor(t, s, "backend-b", movedPath, 100), true)
	if err != nil {
		t.Fatalf("PlaceCopy: %v", err)
	}
	if moved != 100 {
		t.Errorf("moved = %d, want 100", moved)
	}
	assertQueuedOnly(t, s, cleanup, "backend-a", firstWritePath, testMoveReasons.SourceDelete, 100)
	if n := queryPendingCount(t, s); n != 0 {
		t.Errorf("pending intents = %d, want the placed intent cleared", n)
	}

	got := readBackOne(t, s, placedKey)
	if got.BackendName != "backend-b" || got.StorageKey != movedPath {
		t.Errorf("moved copy at %s/%s, want backend-b/%s", got.BackendName, got.StorageKey, movedPath)
	}
	if !got.CreatedAt.Equal(src.CreatedAt) {
		t.Errorf("CreatedAt = %v, want the source's %v", got.CreatedAt, src.CreatedAt)
	}
	if got.Identity == nil || got.Identity.ETag != src.Identity.ETag {
		t.Errorf("Identity = %+v, want the source's %+v", got.Identity, src.Identity)
	}
	assertFormPreserved(t, got, fullyPopulatedForm())
	if size, level := probeOn(t, s, placedKey, "backend-b"); size != 90 || level != "default" {
		t.Errorf("probe = (%d, %q), want (90, \"default\")", size, level)
	}
	if used := quotaBytesUsed(t, s, "backend-a"); used != 0 {
		t.Errorf("backend-a bytes = %d, want 0 after the move", used)
	}
	if used := quotaBytesUsed(t, s, "backend-b"); used != 100 {
		t.Errorf("backend-b bytes = %d, want 100 after the move", used)
	}
}

// TestPlaceCopy_ReplicaKeepsTheSource verifies a placement without
// RemoveSource adds the copy, queues nothing, and leaves the source row and
// its bytes alone.
func TestPlaceCopy_ReplicaKeepsTheSource(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	recordWrite(t, s, firstWritePath, 100)
	src := storetest.LocationOn(t, s, placedKey, "backend-a")

	placed, cleanup, err := place(s, src, claimFor(t, s, "backend-b", replicaPath, 100), false)
	if err != nil {
		t.Fatalf("PlaceCopy: %v", err)
	}
	if placed != 100 {
		t.Errorf("placed = %d, want 100", placed)
	}
	if len(cleanup) != 0 || len(queuedCleanups(t, s)) != 0 {
		t.Errorf("cleanup = %+v, want nothing queued for a replica", cleanup)
	}

	if kept := storetest.LocationOn(t, s, placedKey, "backend-a"); kept.StorageKey != src.StorageKey {
		t.Errorf("source storage key = %q, want %q", kept.StorageKey, src.StorageKey)
	}
	replica := storetest.LocationOn(t, s, placedKey, "backend-b")
	if replica.StorageKey != replicaPath || !replica.CreatedAt.Equal(src.CreatedAt) {
		t.Errorf("replica = %s at %v, want %s at the source's %v",
			replica.StorageKey, replica.CreatedAt, replicaPath, src.CreatedAt)
	}
	if used := quotaBytesUsed(t, s, "backend-a"); used != 100 {
		t.Errorf("backend-a bytes = %d, want 100", used)
	}
	if used := quotaBytesUsed(t, s, "backend-b"); used != 100 {
		t.Errorf("backend-b bytes = %d, want 100", used)
	}
}

// -------------------------------------------------------------------------
// IN-PLACE REWRITES
// -------------------------------------------------------------------------

// rewrittenPath is where an in-place rewrite stores the transformed bytes.
const rewrittenPath = placedKey + "!rw"

// rewriteIntent is the claim an in-place rewrite of placedKey takes on
// backend-a: a plaintext compressed form of size bytes at rewrittenPath.
func rewriteIntent(size int64) *core.PendingObject {
	return &core.PendingObject{
		IntentID: "intent-rewrite", ObjectKey: placedKey, StorageKey: rewrittenPath,
		BackendName: "backend-a", SizeBytes: size, Role: core.PendingRoleCompanion,
		StoredForm: core.StoredForm{CompressionAlgorithm: "zstd", CompressionLevel: "default", CompressionFormatVersion: 1, LogicalSize: 100},
	}
}

// claimRewrite claims the rewrite intent of size bytes the way a rewrite
// pass does, reporting whether backend-a admitted it.
func claimRewrite(t *testing.T, s *Store, size int64) (*core.PendingObject, bool) {
	t.Helper()
	p := rewriteIntent(size)
	fits, err := s.InsertPendingIfFits(context.Background(), p, core.AcceptForRewrite)
	if err != nil {
		t.Fatalf("InsertPendingIfFits(rewrite): %v", err)
	}
	return p, fits
}

// rewriteInPlace claims and places the copy on backend-a back onto backend-a
// at rewrittenPath, carrying the rewrite intent's form of size bytes.
func rewriteInPlace(t *testing.T, s *Store, size int64) (int64, []core.CleanupItem, error) {
	t.Helper()
	intent, fits := claimRewrite(t, s, size)
	if !fits {
		t.Fatalf("backend-a refused the %d byte rewrite claim", size)
	}
	return s.PlaceCopy(context.Background(), &core.Placement{
		Source:       storetest.LocationOn(t, s, placedKey, "backend-a"),
		Intent:       intent,
		RemoveSource: true,
		Reformed:     true,
		Reasons:      testMoveReasons,
	})
}

// TestPlaceCopy_InPlaceRewriteReplacesTheForm verifies a rewrite onto the
// copy's own backend replaces the row at a new path with the new form and
// size, keeps the source's hash, write time and identity, drops the
// compression probe, queues the old copy's bytes, and moves the backend's
// count by the size change.
func TestPlaceCopy_InPlaceRewriteReplacesTheForm(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	recordWrite(t, s, firstWritePath, 100)
	mustPatchCopy(t, s, placedKey, "backend-a", &core.CopyPatch{Probe: &core.CompressionProbe{Size: 90, Level: "default"}})
	backdateCreatedAt(t, s, placedKey, "backend-a", 24*time.Hour)
	src := storetest.LocationOn(t, s, placedKey, "backend-a")

	placed, cleanup, err := rewriteInPlace(t, s, 40)
	if err != nil {
		t.Fatalf("PlaceCopy: %v", err)
	}
	if placed != 40 {
		t.Errorf("placed = %d, want 40", placed)
	}
	assertQueuedOnly(t, s, cleanup, "backend-a", firstWritePath, testMoveReasons.SourceDelete, 100)
	got := readBackOne(t, s, placedKey)
	if got.BackendName != "backend-a" || got.StorageKey != rewrittenPath || got.SizeBytes != 40 {
		t.Errorf("rewritten copy = %s/%s at %d bytes, want backend-a/%s at 40",
			got.BackendName, got.StorageKey, got.SizeBytes, rewrittenPath)
	}
	assertFormPreserved(t, got, &core.StoredForm{
		ContentHash: src.ContentHash, CompressionAlgorithm: "zstd", CompressionLevel: "default",
		CompressionFormatVersion: 1, LogicalSize: 100,
	})
	if !got.CreatedAt.Equal(src.CreatedAt) {
		t.Errorf("CreatedAt = %v, want the source's %v", got.CreatedAt, src.CreatedAt)
	}
	if got.Identity == nil || got.Identity.ETag != src.Identity.ETag {
		t.Errorf("Identity = %+v, want the source's %+v", got.Identity, src.Identity)
	}
	if size, level := probeOn(t, s, placedKey, "backend-a"); size != 0 || level != "" {
		t.Errorf("probe = (%d, %q), want cleared", size, level)
	}
	if used := quotaBytesUsed(t, s, "backend-a"); used != 40 {
		t.Errorf("backend-a bytes = %d, want 40 after the rewrite", used)
	}
}

// TestPlaceCopy_InPlaceRewriteClaimsTheWholeCopy verifies the claim an
// in-place rewrite takes needs room for the whole new copy beside the one it
// replaces, since both occupy the backend until the old bytes are deleted.
func TestPlaceCopy_InPlaceRewriteClaimsTheWholeCopy(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if err := s.SyncQuotaLimits(context.Background(), []config.BackendConfig{{Name: "backend-a", QuotaBytes: 150}}); err != nil {
		t.Fatalf("SyncQuotaLimits: %v", err)
	}
	recordWrite(t, s, firstWritePath, 100)

	if _, fits := claimRewrite(t, s, 60); fits {
		t.Fatal("backend-a admitted a 60 byte rewrite into 50 bytes of room")
	}
	if _, _, err := rewriteInPlace(t, s, 50); err != nil {
		t.Fatalf("PlaceCopy of a 50 byte rewrite into 50 of room: %v", err)
	}
	if used := quotaBytesUsed(t, s, "backend-a"); used != 50 {
		t.Errorf("backend-a bytes = %d, want 50", used)
	}
}

// TestPlaceCopy_InPlaceRewriteOnFlaggedBackend verifies a cordoned backend
// still admits a rewrite of a copy it holds, while a draining one refuses the
// claim.
func TestPlaceCopy_InPlaceRewriteOnFlaggedBackend(t *testing.T) {
	t.Parallel()
	t.Run("cordoned", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		recordWrite(t, s, firstWritePath, 100)
		if _, err := s.SetBackendFlag(context.Background(), "backend-a", core.FlagCordoned); err != nil {
			t.Fatalf("cordon: %v", err)
		}
		if _, _, err := rewriteInPlace(t, s, 40); err != nil {
			t.Fatalf("PlaceCopy on a cordoned backend: %v", err)
		}
	})
	t.Run("draining", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		recordWrite(t, s, firstWritePath, 100)
		mustStartDrain(t, s, "backend-a")
		if _, fits := claimRewrite(t, s, 40); fits {
			t.Error("a draining backend admitted a rewrite claim")
		}
	})
}

// TestPlaceCopy_InPlaceWithoutRemoveIsRefused verifies a placement onto the
// source's own backend that would keep the source records nothing and queues
// the copied bytes.
func TestPlaceCopy_InPlaceWithoutRemoveIsRefused(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	recordWrite(t, s, firstWritePath, 100)
	before, _ := s.GetAllObjectLocations(context.Background(), placedKey)

	intent := claimFor(t, s, "backend-a", rewrittenPath, 100)
	_, cleanup, err := place(s, storetest.LocationOn(t, s, placedKey, "backend-a"), intent, false)
	assertDeclined(t, s, err, core.ErrTargetRefused, before, 100, 0)
	assertIntentQueued(t, s, cleanup, intent, testMoveReasons.Orphan)
}
