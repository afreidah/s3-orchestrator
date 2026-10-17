// -------------------------------------------------------------------------------
// SQLite - Copy Placement Tests
//
// Author: Alex Freidah
//
// Drives PlaceCopy against the real engine: a placement commits only while the
// source is still the copy the caller read and the target admits the bytes, a
// move carries every column of the source row, and a refused placement leaves
// rows and quotas as they were.
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

// -------------------------------------------------------------------------
// DECLINED PLACEMENTS
// -------------------------------------------------------------------------

// TestPlaceCopy_ReplacedSourceIsCopyChanged verifies a move whose source a
// newer write replaced between the read and the placement records nothing.
func TestPlaceCopy_ReplacedSourceIsCopyChanged(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	recordWrite(t, s, firstWritePath, 100)
	read := storetest.LocationOn(t, s, placedKey, "backend-a")
	recordWrite(t, s, placedKey+"!w2", 200)
	before, _ := s.GetAllObjectLocations(context.Background(), placedKey)

	_, err := s.PlaceCopy(context.Background(), &core.Placement{
		Source: read, Target: "backend-b", StorageKey: movedPath, RemoveSource: true,
	})
	assertDeclined(t, s, err, core.ErrCopyChanged, before, 200, 0)
}

// TestPlaceCopy_SourceGoneIsCopyChanged verifies a placement whose source row
// no longer exists records nothing.
func TestPlaceCopy_SourceGoneIsCopyChanged(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, err := s.PlaceCopy(context.Background(), &core.Placement{
		Source:     &core.ObjectLocation{ObjectKey: placedKey, BackendName: "backend-a", StorageKey: firstWritePath},
		Target:     "backend-b",
		StorageKey: movedPath,
	})
	assertDeclined(t, s, err, core.ErrCopyChanged, nil, 0, 0)
}

// TestPlaceCopy_TargetHoldingCopyIsRefused verifies a placement onto a backend
// that already holds a copy of the key records nothing.
func TestPlaceCopy_TargetHoldingCopyIsRefused(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	recordWrite(t, s, firstWritePath, 100)
	mustRecordReplica(t, s, placedKey, "backend-b", "backend-a", 100)
	before, _ := s.GetAllObjectLocations(context.Background(), placedKey)

	_, err := s.PlaceCopy(context.Background(), &core.Placement{
		Source:       storetest.LocationOn(t, s, placedKey, "backend-a"),
		Target:       "backend-b",
		StorageKey:   movedPath,
		RemoveSource: true,
	})
	assertDeclined(t, s, err, core.ErrTargetRefused, before, 100, 100)
}

// TestPlaceCopy_TargetOverQuotaIsRefused verifies a placement onto a backend
// without room for the source's bytes records nothing.
func TestPlaceCopy_TargetOverQuotaIsRefused(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if err := s.SyncQuotaLimits(context.Background(), []config.BackendConfig{{Name: "backend-b", QuotaBytes: 50}}); err != nil {
		t.Fatalf("SyncQuotaLimits: %v", err)
	}
	recordWrite(t, s, firstWritePath, 100)
	before, _ := s.GetAllObjectLocations(context.Background(), placedKey)

	_, err := s.PlaceCopy(context.Background(), &core.Placement{
		Source:     storetest.LocationOn(t, s, placedKey, "backend-a"),
		Target:     "backend-b",
		StorageKey: replicaPath,
	})
	assertDeclined(t, s, err, core.ErrTargetRefused, before, 100, 0)
}

// -------------------------------------------------------------------------
// COMMITTED PLACEMENTS
// -------------------------------------------------------------------------

// TestPlaceCopy_MoveCarriesTheSourceRow verifies a move lands the source row
// on the target unchanged but for backend and path, removes the source row,
// and moves the bytes between the backends' counts. Last-Modified surviving
// the move is the reason created_at is carried.
func TestPlaceCopy_MoveCarriesTheSourceRow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	recordWrite(t, s, firstWritePath, 100)
	mustPatchCopy(t, s, placedKey, "backend-a", &core.CopyPatch{Probe: &core.CompressionProbe{Size: 90, Level: "default"}})
	backdateCreatedAt(t, s, placedKey, "backend-a", 24*time.Hour)
	src := storetest.LocationOn(t, s, placedKey, "backend-a")

	moved, err := s.PlaceCopy(context.Background(), &core.Placement{
		Source: src, Target: "backend-b", StorageKey: movedPath, RemoveSource: true,
	})
	if err != nil {
		t.Fatalf("PlaceCopy: %v", err)
	}
	if moved != 100 {
		t.Errorf("moved = %d, want 100", moved)
	}

	got := readBackOne(t, s, placedKey)
	if got.BackendName != "backend-b" || got.StorageKey != movedPath {
		t.Errorf("moved copy at %s/%s, want backend-b/%s", got.BackendName, got.StorageKey, movedPath)
	}
	if !got.CreatedAt.Equal(src.CreatedAt) {
		t.Errorf("CreatedAt = %v, want the source's %v", got.CreatedAt, src.CreatedAt)
	}
	if got.RecordedETag() != src.RecordedETag() {
		t.Errorf("ETag = %q, want the source's %q", got.RecordedETag(), src.RecordedETag())
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
// RemoveSource adds the copy and leaves the source row and its bytes alone.
func TestPlaceCopy_ReplicaKeepsTheSource(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	recordWrite(t, s, firstWritePath, 100)
	src := storetest.LocationOn(t, s, placedKey, "backend-a")

	placed, err := s.PlaceCopy(context.Background(), &core.Placement{
		Source: src, Target: "backend-b", StorageKey: replicaPath,
	})
	if err != nil {
		t.Fatalf("PlaceCopy: %v", err)
	}
	if placed != 100 {
		t.Errorf("placed = %d, want 100", placed)
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
