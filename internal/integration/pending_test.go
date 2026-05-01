// -------------------------------------------------------------------------------
// Integration Tests - Pending-Row PUT Pattern
//
// Author: Alex Freidah
//
// End-to-end coverage for the PUT-before-COMMIT pending-row pattern:
//   - Success path: pending row inserted then atomically cleared by the
//     metadata commit (DB shows zero pending rows after a clean PUT).
//   - DB-blip recovery: a metadata commit failure leaves the backend bytes
//     and the pending intent untouched; a reaper tick later promotes the
//     intent into object_locations rather than deleting the only copy
//     (the original data-loss bug from issue #657).
//   - Reaper drop branch: an intent whose backend HEAD returns 404 is
//     removed without a corresponding object_locations row.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/afreidah/s3-orchestrator/internal/store"
)

// queryPendingCount returns the number of pending intents for a key.
func queryPendingCount(t *testing.T, key string) int {
	t.Helper()
	var count int
	err := testDB.QueryRow(
		"SELECT COUNT(*) FROM pending_objects WHERE object_key = $1", internalKey(key),
	).Scan(&count)
	if err != nil {
		t.Fatalf("queryPendingCount(%q): %v", key, err)
	}
	return count
}

// backdatePendingRows shifts every pending_objects.created_at into the past
// so the reaper's min-age guard does not block the next tick.
func backdatePendingRows(t *testing.T) {
	t.Helper()
	if _, err := testDB.Exec("UPDATE pending_objects SET created_at = NOW() - INTERVAL '10 minutes'"); err != nil {
		t.Fatalf("backdatePendingRows: %v", err)
	}
}

// runReaperTick backdates every pending row and runs one reaper iteration.
// Returns (resolved, failed) the same way ProcessPendingQueue does.
func runReaperTick(t *testing.T) (resolved, failed int) {
	t.Helper()
	if testManager.PendingReaper == nil {
		t.Fatal("test manager has no PendingReaper wired")
	}
	backdatePendingRows(t)
	return testManager.PendingReaper.ProcessPendingQueue(context.Background())
}

// TestPending_HappyPath verifies that a successful PUT clears the pending
// intent in the same transaction that records the object location.
func TestPending_HappyPath(t *testing.T) {
	resetState(t)

	client := newS3Client(t)
	ctx := context.Background()
	key := uniqueKey(t, "pending-happy")

	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader([]byte("hello")),
		ContentLength: aws.Int64(5),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	if got := queryObjectCopies(t, key); got != 1 {
		t.Errorf("object_locations rows = %d, want 1", got)
	}
	if got := queryPendingCount(t, key); got != 0 {
		t.Errorf("pending_objects rows = %d, want 0 (cleared by atomic commit)", got)
	}
}

// TestPending_DBBlipMidPUT_RecoveredByReaper is the regression test for
// issue #657. With the old code path a metadata commit failure mid-PUT
// would delete the just-uploaded bytes, losing the only copy of an
// overwritten key. The pending pattern preserves the bytes, leaves a
// pending row, and lets the reaper promote it on the next tick.
func TestPending_DBBlipMidPUT_RecoveredByReaper(t *testing.T) {
	resetState(t)

	client := newS3Client(t)
	ctx := context.Background()
	key := uniqueKey(t, "pending-blip")

	// Arm a one-shot failure on the atomic commit. The backend PUT and
	// the InsertPending that precedes it both succeed; only the
	// RecordObjectAndClearPending call returns errSimulatedDBFailure.
	//
	// Bypass the S3 client here: the AWS SDK retries 5xx responses and
	// would consume the one-shot flag on the first attempt, then succeed
	// on the retry — masking the failure we're trying to observe.
	testFailableStore.SetFailCommitOnce()

	_, err := testManager.ObjectManager.PutObject(
		ctx, internalKey(key), bytes.NewReader([]byte("payload")), 7, "application/octet-stream", nil,
	)
	if err == nil {
		t.Fatal("expected PutObject to fail after armed commit failure")
	}

	// The bytes must remain on the backend — the whole point of the
	// pending-row pattern is to never delete an only-copy on commit
	// failure. There is no per-key API to query the backend directly,
	// so we infer presence from the reaper's later promotion succeeding.
	if got := queryPendingCount(t, key); got != 1 {
		t.Fatalf("pending_objects rows = %d, want 1 (intent left for reaper)", got)
	}
	if got := queryObjectCopies(t, key); got != 0 {
		t.Errorf("object_locations rows = %d, want 0 (commit failed)", got)
	}

	resolved, failed := runReaperTick(t)
	if resolved != 1 || failed != 0 {
		t.Errorf("reaper tick: resolved=%d failed=%d, want resolved=1 failed=0", resolved, failed)
	}

	if got := queryPendingCount(t, key); got != 0 {
		t.Errorf("pending_objects rows after reaper = %d, want 0", got)
	}
	if got := queryObjectCopies(t, key); got != 1 {
		t.Errorf("object_locations rows after reaper = %d, want 1 (promoted)", got)
	}

	// And the bytes are now readable through the proxy — proves the
	// promoted location resolves to a backend that actually holds them.
	out, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject after reaper promote: %v", err)
	}
	defer out.Body.Close()
	body := make([]byte, 7)
	if _, err := out.Body.Read(body); err != nil && err.Error() != "EOF" {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "payload" {
		t.Errorf("body = %q, want %q", string(body), "payload")
	}
}

// TestPending_ReaperDropsIntentWhenBackendHas404 covers the inverse
// branch: an intent whose backend HEAD returns 404 has no bytes to
// recover. The reaper deletes the row outright with no object_locations
// side effect.
func TestPending_ReaperDropsIntentWhenBackendHas404(t *testing.T) {
	resetState(t)

	ctx := context.Background()
	key := uniqueKey(t, "pending-drop")

	// Insert a pending row directly without ever uploading bytes. The
	// reaper's HEAD will fail with 404 and it should drop the row.
	intent := &store.PendingObject{
		IntentID:    "drop-" + strings.TrimPrefix(key, "pending-drop-"),
		ObjectKey:   internalKey(key),
		BackendName: "minio-1",
		SizeBytes:   42,
	}
	if err := testStore.InsertPending(ctx, intent); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}
	if got := queryPendingCount(t, key); got != 1 {
		t.Fatalf("seed: pending_objects rows = %d, want 1", got)
	}

	resolved, failed := runReaperTick(t)
	if resolved != 1 || failed != 0 {
		t.Errorf("reaper tick: resolved=%d failed=%d, want resolved=1 failed=0", resolved, failed)
	}

	if got := queryPendingCount(t, key); got != 0 {
		t.Errorf("pending_objects rows after reaper = %d, want 0", got)
	}
	if got := queryObjectCopies(t, key); got != 0 {
		t.Errorf("object_locations rows after reaper = %d, want 0 (no bytes to promote)", got)
	}
}

// TestPending_ReaperRespectsMinAge verifies that a freshly inserted
// pending row is left alone — the min-age guard exists so the reaper
// does not race a synchronous commit that hasn't yet run.
func TestPending_ReaperRespectsMinAge(t *testing.T) {
	resetState(t)

	ctx := context.Background()
	key := uniqueKey(t, "pending-young")

	intent := &store.PendingObject{
		IntentID:    "young-" + strings.TrimPrefix(key, "pending-young-"),
		ObjectKey:   internalKey(key),
		BackendName: "minio-1",
		SizeBytes:   10,
	}
	if err := testStore.InsertPending(ctx, intent); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}

	// Tick WITHOUT backdating: the row's created_at = NOW() is far
	// younger than the 5m default min-age, so the reaper must skip it.
	resolved, failed := testManager.PendingReaper.ProcessPendingQueue(ctx)
	if resolved != 0 || failed != 0 {
		t.Errorf("reaper tick on young row: resolved=%d failed=%d, want both 0", resolved, failed)
	}
	if got := queryPendingCount(t, key); got != 1 {
		t.Errorf("pending_objects rows = %d, want 1 (young row preserved)", got)
	}

	// Wait briefly to keep the test fast; explicit cleanup avoids
	// leaking the row into later tests if resetState ordering changes.
	_ = time.Now()
	if err := testStore.DeletePending(ctx, intent.IntentID); err != nil {
		t.Fatalf("DeletePending cleanup: %v", err)
	}
}
