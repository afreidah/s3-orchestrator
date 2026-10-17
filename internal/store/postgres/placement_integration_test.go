// -------------------------------------------------------------------------------
// Postgres - Copy Placement Integration Tests
//
// Author: Alex Freidah
//
// Drives PlaceCopy against a real PostgreSQL: a placement commits only while
// the source is still the copy the caller read and the target admits the
// bytes, a move carries every column of the source row, and a refused
// placement leaves rows and quotas as they were. The database is shared across
// tests, so each test places between backends of its own.
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
	t.Cleanup(func() { _, _, _ = s.DeleteObject(context.Background(), f.key) })
	return f
}

// recordWrite records one write of the key on the source backend at the path
// suffix names, stored in every form column and carrying an etag named for it.
func (f *placementFixture) recordWrite(t *testing.T, suffix string, size int64) {
	t.Helper()
	if _, _, err := f.s.RecordObject(context.Background(), &core.RecordObjectRequest{
		Key: f.key, Size: size, Form: fullyPopulatedForm(),
		Copies:   []core.ObjectCopy{{Backend: f.src, StorageKey: f.key + suffix}},
		Identity: &core.ObjectIdentity{ETag: `"` + suffix + `"`},
	}); err != nil {
		t.Fatalf("RecordObject(%s): %v", suffix, err)
	}
}

// place runs PlaceCopy from source onto the target at the path suffix names.
func (f *placementFixture) place(source *core.ObjectLocation, suffix string, removeSource bool) (int64, error) {
	return f.s.PlaceCopy(context.Background(), &core.Placement{
		Source: source, Target: f.dst, StorageKey: f.key + suffix, RemoveSource: removeSource,
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

// -------------------------------------------------------------------------
// DECLINED PLACEMENTS
// -------------------------------------------------------------------------

// TestPgPlaceCopy_ReplacedSourceIsCopyChanged verifies a move whose source a
// newer write replaced between the read and the placement records nothing.
func TestPgPlaceCopy_ReplacedSourceIsCopyChanged(t *testing.T) {
	f := newPlacementFixture(t, 1<<30)
	f.recordWrite(t, firstWriteSuffix, 100)
	read := storetest.LocationOn(t, f.s, f.key, f.src)
	f.recordWrite(t, secondWriteSuffix, 200)
	before := f.copies(t)

	_, err := f.place(read, movedSuffix, true)
	f.assertDeclined(t, err, core.ErrCopyChanged, before, 200, 0)
}

// TestPgPlaceCopy_SourceGoneIsCopyChanged verifies a placement whose source
// row no longer exists records nothing.
func TestPgPlaceCopy_SourceGoneIsCopyChanged(t *testing.T) {
	f := newPlacementFixture(t, 1<<30)
	gone := &core.ObjectLocation{ObjectKey: f.key, BackendName: f.src, StorageKey: f.key + firstWriteSuffix}

	_, err := f.place(gone, replicaSuffix, false)
	f.assertDeclined(t, err, core.ErrCopyChanged, nil, 0, 0)
}

// TestPgPlaceCopy_TargetHoldingCopyIsRefused verifies a placement onto a
// backend that already holds a copy of the key records nothing.
func TestPgPlaceCopy_TargetHoldingCopyIsRefused(t *testing.T) {
	f := newPlacementFixture(t, 1<<30)
	f.recordWrite(t, firstWriteSuffix, 100)
	mustRecordReplica(t, f.s, f.key, f.dst, f.src)
	before := f.copies(t)

	_, err := f.place(storetest.LocationOn(t, f.s, f.key, f.src), movedSuffix, true)
	f.assertDeclined(t, err, core.ErrTargetRefused, before, 100, 100)
}

// TestPgPlaceCopy_TargetOverQuotaIsRefused verifies a placement onto a backend
// without room for the source's bytes records nothing.
func TestPgPlaceCopy_TargetOverQuotaIsRefused(t *testing.T) {
	f := newPlacementFixture(t, 50)
	f.recordWrite(t, firstWriteSuffix, 100)
	before := f.copies(t)

	_, err := f.place(storetest.LocationOn(t, f.s, f.key, f.src), replicaSuffix, false)
	f.assertDeclined(t, err, core.ErrTargetRefused, before, 100, 0)
}

// -------------------------------------------------------------------------
// COMMITTED PLACEMENTS
// -------------------------------------------------------------------------

// TestPgPlaceCopy_MoveCarriesTheSourceRow verifies a move lands the source
// row on the target unchanged but for backend and path, removes the source
// row, and moves the bytes between the backends' counts. Last-Modified
// surviving the move is the reason created_at is carried rather than stamped.
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

	moved, err := f.place(src, movedSuffix, true)
	if err != nil {
		t.Fatalf("PlaceCopy: %v", err)
	}
	if moved != 100 {
		t.Errorf("moved = %d, want 100", moved)
	}

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

// TestPgPlaceCopy_ReplicaKeepsTheSource verifies a placement without
// RemoveSource adds the copy and leaves the source row and its bytes alone.
func TestPgPlaceCopy_ReplicaKeepsTheSource(t *testing.T) {
	f := newPlacementFixture(t, 1<<30)
	f.recordWrite(t, firstWriteSuffix, 100)
	src := storetest.LocationOn(t, f.s, f.key, f.src)

	placed, err := f.place(src, replicaSuffix, false)
	if err != nil {
		t.Fatalf("PlaceCopy: %v", err)
	}
	if placed != 100 {
		t.Errorf("placed = %d, want 100", placed)
	}

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
