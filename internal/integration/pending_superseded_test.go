// -------------------------------------------------------------------------------
// Integration Tests - Superseded Pending Intents
//
// Author: Alex Freidah
//
// Pins what happens to an intent a later write invalidates: the write clears it
// as part of its own transaction and reports its bytes for cleanup, so the
// reaper finds nothing to resolve and the copy the write committed is left
// alone. The transactional logic is covered by unit tests; this exercises the
// full write -> store -> reaper path against real backends.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// TestInt_PendingIntent_ClearedByOverwritingWrite seeds a stale pending intent
// for key K on one backend, then records a real object for K on another. The
// write must remove the intent and hand back its bytes, leaving the reaper with
// nothing to do and the recorded copy untouched.
func TestInt_PendingIntent_ClearedByOverwritingWrite(t *testing.T) {
	resetState(t)

	ctx := context.Background()
	key := uniqueKey(t, "pending-superseded")

	// Seed an old pending intent targeting backend minio-2.
	intent := &core.PendingObject{
		IntentID:    "superseded-" + strings.TrimPrefix(key, "pending-superseded-"),
		ObjectKey:   internalKey(key),
		BackendName: "minio-2",
		SizeBytes:   42,
	}
	if _, err := testStore.InsertPendingIfFits(ctx, intent); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}

	// Backdate the pending row so a reaper tick would be willing to touch it,
	// which is what makes the assertion below about the reaper meaningful.
	backdatePendingRows(t)

	// Record a real object_locations row for the same key on a different
	// backend. The intent describes an object this write replaces.
	displaced, _, err := testStore.RecordObject(ctx, &core.RecordObjectRequest{
		Key: internalKey(key), Copies: []core.ObjectCopy{{Backend: "minio-1"}}, Size: 100,
	})
	if err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	// The intent's bytes come back for cleanup, labelled so an operator reading
	// the cleanup queue can tell them from a displaced copy.
	var found bool
	for _, dc := range displaced {
		if dc.BackendName == "minio-2" && dc.SizeBytes == 42 && dc.Reason == core.CleanupReasonSupersededIntent {
			found = true
		}
	}
	if !found {
		t.Errorf("write did not report the superseded intent's bytes: %+v", displaced)
	}

	if got := queryObjectCopies(t, key); got != 1 {
		t.Fatalf("object_locations rows = %d, want 1", got)
	}
	if got := queryPendingCount(t, key); got != 0 {
		t.Fatalf("pending_objects rows after the write = %d, want 0 (the write clears them)", got)
	}

	// Nothing is left for the reaper, which is the point: it exists for a
	// process that died, not for intents a later write already settled.
	pendSum := testWorkers.PendingReaper.ProcessPendingQueue(ctx)
	if pendSum.Succeeded != 0 || pendSum.Failed != 0 {
		t.Errorf("reaper tick: resolved=%d failed=%d, want both 0", pendSum.Succeeded, pendSum.Failed)
	}

	// The recorded copy must be exactly one and still on its original backend.
	if got := queryObjectCopies(t, key); got != 1 {
		t.Errorf("object_locations after reaper = %d, want 1", got)
	}
	if backends := queryObjectBackends(t, key); len(backends) != 1 || backends[0] != "minio-1" {
		t.Errorf("backends after reaper = %v, want [minio-1]", backends)
	}
}
