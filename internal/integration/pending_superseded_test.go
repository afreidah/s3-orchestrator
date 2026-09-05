// -------------------------------------------------------------------------------
// Integration Tests - Pending Reaper Superseded Path
//
// Author: Alex Freidah
//
// Pins the timestamp-aware supersession contract end-to-end: a pending
// intent whose target backend later has the bytes is dropped (not
// promoted) when a newer object_locations row exists for the same key.
// The transactional logic in core.promotePendingTx is well-covered by
// unit tests, but the full reaper -> store -> outcome path was not
// exercised under a realistic concurrent-overwrite scenario.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// TestInt_PendingReaper_SupersededByNewerLocation enqueues a stale
// pending intent for key K, then writes a newer object_locations row
// for the same K via RecordObject. The reaper tick must drop the
// intent without promoting it (Superseded path) and without disturbing
// the newer location row.
func TestInt_PendingReaper_SupersededByNewerLocation(t *testing.T) {
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

	// Backdate the pending row so the reaper's min-age guard lets it
	// through AND so its created_at is older than the soon-to-be-inserted
	// object_locations row.
	backdatePendingRows(t)

	// Now record a real object_locations row for the same key on a
	// different backend (minio-1). Its created_at = NOW() is newer than
	// the backdated pending row, satisfying intentSuperseded.
	if _, _, err := testStore.RecordObject(ctx, &core.RecordObjectRequest{Key: internalKey(key), Backend: "minio-1", Size: 100}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if got := queryObjectCopies(t, key); got != 1 {
		t.Fatalf("seed object_locations rows = %d, want 1", got)
	}
	if got := queryPendingCount(t, key); got != 1 {
		t.Fatalf("seed pending_objects rows = %d, want 1", got)
	}

	// Run the reaper directly (skip runReaperTick's backdate; we
	// already backdated above and want to preserve the NOW() created_at
	// on the object_locations row).
	pendSum := testWorkers.PendingReaper.ProcessPendingQueue(ctx)
	resolved, failed := pendSum.Succeeded, pendSum.Failed
	if resolved != 1 || failed != 0 {
		t.Errorf("reaper tick: resolved=%d failed=%d, want resolved=1 failed=0", resolved, failed)
	}

	// Pending row must be gone (dropped as Superseded).
	if got := queryPendingCount(t, key); got != 0 {
		t.Errorf("pending_objects after reaper = %d, want 0", got)
	}

	// The newer object_locations row must still be exactly one and
	// still point at the original backend, untouched by the reaper.
	if got := queryObjectCopies(t, key); got != 1 {
		t.Errorf("object_locations after reaper = %d, want 1 (Superseded must not create a second copy)", got)
	}
	if backends := queryObjectBackends(t, key); len(backends) != 1 || backends[0] != "minio-1" {
		t.Errorf("backends after reaper = %v, want [minio-1] (Superseded must not displace existing copy)", backends)
	}
}
