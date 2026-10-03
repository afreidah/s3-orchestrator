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
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

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
	if _, _, err := store.RecordObject(ctx, &core.RecordObjectRequest{
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
	if _, inserted, err := store.RecordReplica(ctx, &core.ReplicaInsert{
		ObjectKey: key, TargetBackend: "b2", SourceBackend: "b1", StorageKey: key + "!w2",
	}); err != nil || !inserted {
		t.Fatalf("RecordReplica for the write's copy = %v, %v", inserted, err)
	}

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
