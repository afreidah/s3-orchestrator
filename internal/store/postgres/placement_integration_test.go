// -------------------------------------------------------------------------------
// Postgres - Copy Placement Integration Tests
//
// Author: Alex Freidah
//
// Drives PlaceCopy against a real PostgreSQL: a placement commits only while
// its intent is held and the source is still the copy the caller read, a move
// carries every column of the source row and queues the source's bytes, and a
// refused placement leaves rows and quotas as they were while queueing the
// bytes the caller copied. The database is shared across tests, so each test
// places between backends of its own.
// -------------------------------------------------------------------------------

//go:build integration

package postgres

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// Path suffixes for a placed key: where its first and second writes land,
// and where a move and a replica write the copied bytes.
const (
	firstWriteSuffix  = "!w1"
	secondWriteSuffix = "!w2"
	movedSuffix       = "!m"
	replicaSuffix     = "!r"
)

// The reasons a test placement queues bytes under.
var testMoveReasons = core.MoveReasons{Orphan: "test_orphan", SourceDelete: "test_source_delete"}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// placementFixture is one test's key and the two backends it places between.
type placementFixture struct {
	s        *Store
	key      string
	src, dst string
}

// newPlacementFixture registers a source and a target backend unique to the
// test, the target with dstQuota bytes, and deletes the key when it ends.
func newPlacementFixture(t *testing.T, dstQuota int64) *placementFixture {
	t.Helper()
	s := adapterPgStore(t)
	f := &placementFixture{s: s, key: uniqueKey(t, "placed"), src: uniqueKey(t, "src"), dst: uniqueKey(t, "dst")}
	if err := s.SyncQuotaLimits(context.Background(), []config.BackendConfig{
		{Name: f.src, QuotaBytes: 1 << 30},
		{Name: f.dst, QuotaBytes: dstQuota},
	}); err != nil {
		t.Fatalf("SyncQuotaLimits: %v", err)
	}
	t.Cleanup(func() { _, _ = s.DeleteObject(context.Background(), f.key) })
	return f
}

// recordWrite records one write of the key on the source backend at the path
// suffix names, stored in every form column and carrying an etag named for it.
func (f *placementFixture) recordWrite(t *testing.T, suffix string, size int64) {
	t.Helper()
	if _, err := f.s.RecordObject(context.Background(), &core.RecordObjectRequest{
		Key: f.key, Size: size, Form: fullyPopulatedForm(),
		Copies:   []core.ObjectCopy{{Backend: f.src, StorageKey: f.key + suffix}},
		Identity: &core.ObjectIdentity{ETag: `"` + suffix + `"`},
	}); err != nil {
		t.Fatalf("RecordObject(%s): %v", suffix, err)
	}
}

// claim claims the target for a copy of source written at the path suffix
// names, the way a copy's caller claims before writing.
func (f *placementFixture) claim(t *testing.T, source *core.ObjectLocation, suffix string) *core.PendingObject {
	t.Helper()
	return claimCopyIntent(t, f.s, f.key, f.dst, f.key+suffix, source.SizeBytes)
}

// place claims the target and runs PlaceCopy from source onto it at the path
// suffix names.
func (f *placementFixture) place(t *testing.T, source *core.ObjectLocation, suffix string, removeSource bool) (int64, []core.CleanupItem, error) {
	t.Helper()
	return f.s.PlaceCopy(context.Background(), &core.Placement{
		Source: source, Intent: f.claim(t, source, suffix), RemoveSource: removeSource, Reasons: testMoveReasons,
	})
}

// copies returns the key's copies, empty when it has none.
func (f *placementFixture) copies(t *testing.T) []core.ObjectLocation {
	t.Helper()
	locs, err := f.s.GetAllObjectLocations(context.Background(), f.key)
	if err != nil && !errors.Is(err, core.ErrObjectNotFound) {
		t.Fatalf("GetAllObjectLocations: %v", err)
	}
	return locs
}

// assertDeclined checks a placement failed with want and left the copy set
// and both backends' byte counts as they were before it.
func (f *placementFixture) assertDeclined(t *testing.T, err, want error, before []core.ObjectLocation, usedSrc, usedDst int64) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("PlaceCopy = %v, want %v", err, want)
	}
	after := f.copies(t)
	if len(after) != len(before) {
		t.Fatalf("copies = %+v, want the %d held before", after, len(before))
	}
	for i := range before {
		if after[i].BackendName != before[i].BackendName || after[i].StorageKey != before[i].StorageKey {
			t.Errorf("copy %d = %s/%s, want %s/%s", i, after[i].BackendName, after[i].StorageKey,
				before[i].BackendName, before[i].StorageKey)
		}
	}
	if got := readBytesUsed(t, f.s, f.src); got != usedSrc {
		t.Errorf("source bytes = %d, want %d", got, usedSrc)
	}
	if got := readBytesUsed(t, f.s, f.dst); got != usedDst {
		t.Errorf("target bytes = %d, want %d", got, usedDst)
	}
}

// assertQueued checks the placement returned exactly one cleanup row, for
// the bytes at path on backend under reason, and that the row was committed.
func assertQueued(t *testing.T, s *Store, cleanup []core.CleanupItem, backend, path, reason string) {
	t.Helper()
	if len(cleanup) != 1 {
		t.Fatalf("cleanup = %+v, want one row", cleanup)
	}
	got := cleanup[0]
	if got.BackendName != backend || got.StorageKey != path || got.Reason != reason {
		t.Errorf("cleanup = %s/%s (%s), want %s/%s (%s)", got.BackendName, got.StorageKey, got.Reason, backend, path, reason)
	}
	if n := pgCount(t, s, `SELECT COUNT(*) FROM cleanup_queue WHERE id = $1`, got.ID); n != 1 {
		t.Errorf("cleanup row %d committed %d times, want 1", got.ID, n)
	}
}

// assertIntentGone checks the placement cleared its intent.
func assertIntentGone(t *testing.T, s *Store, p *core.PendingObject) {
	t.Helper()
	if pgCount(t, s, `SELECT COUNT(*) FROM pending_objects WHERE intent_id = $1`, p.IntentID) != 0 {
		t.Errorf("intent %s still recorded", p.IntentID)
	}
}

// -------------------------------------------------------------------------
// DECLINED PLACEMENTS
// -------------------------------------------------------------------------

// TestPgPlaceCopy_ReplacedSourceIsCopyChanged verifies a move whose source a
// newer write replaced between the read and the placement records nothing and
// queues the copied bytes under the orphan reason.
func TestPgPlaceCopy_ReplacedSourceIsCopyChanged(t *testing.T) {
	f := newPlacementFixture(t, 1<<30)
	f.recordWrite(t, firstWriteSuffix, 100)
	read := storetest.LocationOn(t, f.s, f.key, f.src)
	f.recordWrite(t, secondWriteSuffix, 200)
	before := f.copies(t)

	_, cleanup, err := f.place(t, read, movedSuffix, true)
	f.assertDeclined(t, err, core.ErrCopyChanged, before, 200, 0)
	assertQueued(t, f.s, cleanup, f.dst, f.key+movedSuffix, testMoveReasons.Orphan)
}

// TestPgPlaceCopy_SourceGoneIsCopyChanged verifies a placement whose source
// row no longer exists records nothing and queues the copied bytes.
func TestPgPlaceCopy_SourceGoneIsCopyChanged(t *testing.T) {
	f := newPlacementFixture(t, 1<<30)
	gone := &core.ObjectLocation{ObjectKey: f.key, BackendName: f.src, StorageKey: f.key + firstWriteSuffix, SizeBytes: 10}

	_, cleanup, err := f.place(t, gone, replicaSuffix, false)
	f.assertDeclined(t, err, core.ErrCopyChanged, nil, 0, 0)
	assertQueued(t, f.s, cleanup, f.dst, f.key+replicaSuffix, testMoveReasons.Orphan)
}

// TestPgPlaceCopy_TargetHoldingCopyIsRefused verifies a placement onto a
// backend that already holds a copy of the key records nothing, drops its
// intent, and queues the copied bytes.
func TestPgPlaceCopy_TargetHoldingCopyIsRefused(t *testing.T) {
	f := newPlacementFixture(t, 1<<30)
	f.recordWrite(t, firstWriteSuffix, 100)
	mustRecordReplica(t, f.s, f.key, f.dst, f.src)
	before := f.copies(t)
	src := storetest.LocationOn(t, f.s, f.key, f.src)
	intent := f.claim(t, src, movedSuffix)

	_, cleanup, err := f.s.PlaceCopy(context.Background(), &core.Placement{
		Source: src, Intent: intent, RemoveSource: true, Reasons: testMoveReasons,
	})
	f.assertDeclined(t, err, core.ErrTargetRefused, before, 100, 100)
	assertQueued(t, f.s, cleanup, f.dst, f.key+movedSuffix, testMoveReasons.Orphan)
	assertIntentGone(t, f.s, intent)
}

// TestPgPlaceCopy_LostIntentQueuesSupersededBytes verifies a placement whose
// intent a client write cleared mid-copy records nothing and queues the
// copied bytes as a superseded intent's.
func TestPgPlaceCopy_LostIntentQueuesSupersededBytes(t *testing.T) {
	f := newPlacementFixture(t, 1<<30)
	f.recordWrite(t, firstWriteSuffix, 100)
	before := f.copies(t)
	src := storetest.LocationOn(t, f.s, f.key, f.src)
	intent := f.claim(t, src, replicaSuffix)
	if err := f.s.DeletePending(context.Background(), intent.IntentID); err != nil {
		t.Fatalf("DeletePending: %v", err)
	}

	_, cleanup, err := f.s.PlaceCopy(context.Background(), &core.Placement{Source: src, Intent: intent, Reasons: testMoveReasons})
	f.assertDeclined(t, err, core.ErrCopyChanged, before, 100, 0)
	assertQueued(t, f.s, cleanup, f.dst, f.key+replicaSuffix, core.CleanupReasonSupersededIntent)
}

// TestPgPlaceCopy_TargetOverQuotaIsRefusedAtTheClaim verifies a target
// without room for the source's bytes declines the intent, so the copy is
// never written and nothing needs placing.
func TestPgPlaceCopy_TargetOverQuotaIsRefusedAtTheClaim(t *testing.T) {
	f := newPlacementFixture(t, 50)
	f.recordWrite(t, firstWriteSuffix, 100)

	fits, err := f.s.InsertPendingIfFits(context.Background(), &core.PendingObject{
		IntentID: uniqueKey(t, "intent"), ObjectKey: f.key, StorageKey: f.key + replicaSuffix,
		BackendName: f.dst, SizeBytes: 100, Role: core.PendingRoleCompanion,
	}, core.AcceptForWrite)
	if err != nil || fits {
		t.Fatalf("InsertPendingIfFits = %v, %v; want declined", fits, err)
	}
}

// -------------------------------------------------------------------------
// COMMITTED PLACEMENTS
// -------------------------------------------------------------------------

// TestPgPlaceCopy_MoveCarriesTheSourceRow verifies a move lands the source
// row on the target unchanged but for backend and path, removes the source
// row and queues its bytes, and moves the bytes between the backends' counts.
// Last-Modified surviving the move is the reason created_at is carried rather
// than stamped.
func TestPgPlaceCopy_MoveCarriesTheSourceRow(t *testing.T) {
	f := newPlacementFixture(t, 1<<30)
	ctx := context.Background()
	f.recordWrite(t, firstWriteSuffix, 100)
	if err := f.s.UpdateCopyMetadata(ctx, storetest.LocationOn(t, f.s, f.key, f.src),
		&core.CopyPatch{Probe: &core.CompressionProbe{Size: 90, Level: "default"}}); err != nil {
		t.Fatalf("UpdateCopyMetadata: %v", err)
	}
	if _, err := f.s.pool.Exec(ctx,
		`UPDATE object_locations SET created_at = NOW() - interval '24 hours' WHERE object_key = $1`, f.key,
	); err != nil {
		t.Fatalf("backdate created_at: %v", err)
	}
	src := storetest.LocationOn(t, f.s, f.key, f.src)

	moved, cleanup, err := f.place(t, src, movedSuffix, true)
	if err != nil {
		t.Fatalf("PlaceCopy: %v", err)
	}
	if moved != 100 {
		t.Errorf("moved = %d, want 100", moved)
	}
	assertQueued(t, f.s, cleanup, f.src, f.key+firstWriteSuffix, testMoveReasons.SourceDelete)

	got := readBackOne(t, f.s, f.key)
	if got.BackendName != f.dst || got.StorageKey != f.key+movedSuffix {
		t.Errorf("moved copy at %s/%s, want %s/%s", got.BackendName, got.StorageKey, f.dst, f.key+movedSuffix)
	}
	if !got.CreatedAt.Equal(src.CreatedAt) {
		t.Errorf("CreatedAt = %v, want the source's %v", got.CreatedAt, src.CreatedAt)
	}
	if !reflect.DeepEqual(got.Identity, src.Identity) {
		t.Errorf("Identity = %+v, want the source's %+v", got.Identity, src.Identity)
	}
	assertFormPreserved(t, got, fullyPopulatedForm())
	if size, level := pgProbeOn(t, f.s, f.key, f.dst); size != 90 || level != "default" {
		t.Errorf("probe = (%d, %q), want (90, \"default\")", size, level)
	}
	if used := readBytesUsed(t, f.s, f.src); used != 0 {
		t.Errorf("source bytes = %d, want 0 after the move", used)
	}
	if used := readBytesUsed(t, f.s, f.dst); used != 100 {
		t.Errorf("target bytes = %d, want 100 after the move", used)
	}
}

// TestPgPlaceCopy_MoveKeepsAnUnmanagedCopyUnmanaged verifies a copy placed
// from an unmanaged row is recorded unmanaged too, so moving it does not turn
// bytes outside every bucket prefix into an object workers act on.
func TestPgPlaceCopy_MoveKeepsAnUnmanagedCopyUnmanaged(t *testing.T) {
	f := newPlacementFixture(t, 1<<30)
	if _, err := f.s.ImportObject(context.Background(), &core.ImportObjectRequest{
		Key: f.key, Backend: f.src, Size: 100, Unmanaged: true,
	}); err != nil {
		t.Fatalf("ImportObject: %v", err)
	}
	src := storetest.LocationOn(t, f.s, f.key, f.src)

	if _, _, err := f.place(t, src, movedSuffix, true); err != nil {
		t.Fatalf("PlaceCopy: %v", err)
	}
	if got := readBackOne(t, f.s, f.key); !got.Unmanaged {
		t.Errorf("moved copy on %s is managed, want it unmanaged like its source", got.BackendName)
	}
}

// TestPgPlaceCopy_ReplicaKeepsTheSource verifies a placement without
// RemoveSource adds the copy, clears its intent, queues nothing, and leaves
// the source row and its bytes alone.
func TestPgPlaceCopy_ReplicaKeepsTheSource(t *testing.T) {
	f := newPlacementFixture(t, 1<<30)
	f.recordWrite(t, firstWriteSuffix, 100)
	src := storetest.LocationOn(t, f.s, f.key, f.src)
	intent := f.claim(t, src, replicaSuffix)

	placed, cleanup, err := f.s.PlaceCopy(context.Background(), &core.Placement{Source: src, Intent: intent, Reasons: testMoveReasons})
	if err != nil {
		t.Fatalf("PlaceCopy: %v", err)
	}
	if placed != 100 || len(cleanup) != 0 {
		t.Errorf("placed = %d with cleanup %+v, want 100 and none", placed, cleanup)
	}
	assertIntentGone(t, f.s, intent)

	if kept := storetest.LocationOn(t, f.s, f.key, f.src); kept.StorageKey != src.StorageKey {
		t.Errorf("source storage key = %q, want %q", kept.StorageKey, src.StorageKey)
	}
	replica := storetest.LocationOn(t, f.s, f.key, f.dst)
	if replica.StorageKey != f.key+replicaSuffix || !replica.CreatedAt.Equal(src.CreatedAt) {
		t.Errorf("replica = %s at %v, want %s at the source's %v",
			replica.StorageKey, replica.CreatedAt, f.key+replicaSuffix, src.CreatedAt)
	}
	if used := readBytesUsed(t, f.s, f.src); used != 100 {
		t.Errorf("source bytes = %d, want 100", used)
	}
	if used := readBytesUsed(t, f.s, f.dst); used != 100 {
		t.Errorf("target bytes = %d, want 100", used)
	}
}
