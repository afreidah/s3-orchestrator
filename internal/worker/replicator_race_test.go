// -------------------------------------------------------------------------------
// Replicator Race Tests
//
// Author: Alex Freidah
//
// Covers a replica that lands on a backend where a write commits its own copy
// of the same key first. The replicator's row insert loses, and the cleanup of
// its copy must remove only the bytes it wrote, leaving the write's copy and
// row intact.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/sqlite"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// placeWritesCopy records a copy of key on target at path, claimed and placed
// the way a committed write's copy is, from the key's copy on b1.
func placeWritesCopy(t *testing.T, store *sqlite.Store, key, target, path string, size int64) {
	t.Helper()
	ctx := context.Background()
	intent := writepath.NewCopyIntent(key, target, size, nil)
	intent.StorageKey = path
	if ok, err := store.InsertPendingIfFits(ctx, intent, core.AcceptForWrite); err != nil || !ok {
		t.Fatalf("InsertPendingIfFits for the write's copy = %v, %v", ok, err)
	}
	if _, _, err := store.PlaceCopy(ctx, &core.Placement{
		Source: storetest.LocationOn(t, store, key, "b1"), Intent: intent, Reasons: writepath.ReplicationReasons,
	}); err != nil {
		t.Fatalf("PlaceCopy for the write's copy: %v", err)
	}
}

// TestReplicateObject_SupersededCopyLeavesTheWritesCopy hands the replicator a
// copy list read before a write committed its copy on b2. The replicator picks
// b2, uploads there, and loses the insert to the write's row. Its cleanup must
// delete its own path on b2 and nothing else.
func TestReplicateObject_SupersededCopyLeavesTheWritesCopy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const key = "bkt/k"
	data := []byte("data-" + key)

	store := newDrainStore(t, "b1", "b2")
	b1, b2 := backendtest.NewInMemory(), backendtest.NewInMemory()
	b1.Objects[key+"!w1"] = backendtest.Object{Data: data}
	if _, err := store.RecordObject(ctx, &core.RecordObjectRequest{
		Key: key, Size: int64(len(data)),
		Copies: []core.ObjectCopy{{Backend: "b1", StorageKey: key + "!w1"}},
	}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	stale, err := store.GetAllObjectLocations(ctx, key)
	if err != nil {
		t.Fatalf("GetAllObjectLocations: %v", err)
	}

	b2.Objects[key+"!w2"] = backendtest.Object{Data: data}
	placeWritesCopy(t, store, key, "b2", key+"!w2", int64(len(data)))

	r := newReplicatorFor(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": b2}, &fleetOpts{Order: []string{"b1", "b2"}})
	out := r.ReplicateObject(ctx, key, stale, 1)
	if out.Superseded != 1 {
		t.Fatalf("outcome = %+v, want the replica superseded by the write's row", out)
	}

	if !b2.Has(key + "!w2") {
		t.Error("the write's copy on b2 was deleted")
	}
	for k := range b2.Objects {
		if strings.HasPrefix(k, key) && k != key+"!w2" {
			t.Errorf("b2 still holds %s; the superseded replica should have been cleaned up", k)
		}
	}
	locs, err := store.GetAllObjectLocations(ctx, key)
	if err != nil {
		t.Fatalf("GetAllObjectLocations: %v", err)
	}
	for i := range locs {
		if locs[i].BackendName == "b2" && locs[i].StorageKey != key+"!w2" {
			t.Errorf("b2 row points at %s, want the write's path", locs[i].StorageKey)
		}
	}
}
