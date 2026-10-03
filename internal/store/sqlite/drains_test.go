// -------------------------------------------------------------------------------
// Backend Drain Record Tests
//
// Author: Alex Freidah
//
// Drives a drain record through its states and checks that admission refuses
// the backend for as long as a record of any state exists.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// drainBackendName is the backend every drain test drains.
const drainBackendName = "backend-a"

// drainFor returns the named backend's drain record, failing the test when
// there is none.
func drainFor(t *testing.T, s *Store, backend string) core.BackendDrain {
	t.Helper()
	drains, err := s.ListDrains(context.Background())
	if err != nil {
		t.Fatalf("ListDrains: %v", err)
	}
	for _, d := range drains {
		if d.BackendName == backend {
			return d
		}
	}
	t.Fatalf("no drain record for %s in %+v", backend, drains)
	return core.BackendDrain{}
}

// admits reports whether admission accepts a one-byte write on the backend.
func admits(t *testing.T, s *Store, backend, intentID string) bool {
	t.Helper()
	ok, err := s.InsertPendingIfFits(context.Background(), &core.PendingObject{
		IntentID: intentID, ObjectKey: "drain/" + intentID, BackendName: backend, SizeBytes: 1,
	})
	if err != nil {
		t.Fatalf("InsertPendingIfFits: %v", err)
	}
	return ok
}

// mustStartDrain starts a drain of the backend, failing the test unless it
// started.
func mustStartDrain(t *testing.T, s *Store, backend string) {
	t.Helper()
	if started, err := s.StartDrain(context.Background(), backend); err != nil || !started {
		t.Fatalf("StartDrain(%s) = %v, %v; want started", backend, started, err)
	}
}

// mustFailDrain marks the backend's drain failed.
func mustFailDrain(t *testing.T, s *Store, backend, reason string) {
	t.Helper()
	if err := s.MarkDrainFailed(context.Background(), backend, reason); err != nil {
		t.Fatalf("MarkDrainFailed: %v", err)
	}
}

// completes reports whether CompleteDrain marked the backend drained.
func completes(t *testing.T, s *Store, backend string) bool {
	t.Helper()
	done, err := s.CompleteDrain(context.Background(), backend)
	if err != nil {
		t.Fatalf("CompleteDrain: %v", err)
	}
	return done
}

// TestDrainRecord_StartRefusesAdmission verifies a started drain is recorded in
// progress, cannot be started again, and refuses writes to its backend.
func TestDrainRecord_StartRefusesAdmission(t *testing.T) {
	s := newTestStore(t)
	mustStartDrain(t, s, drainBackendName)

	if d := drainFor(t, s, drainBackendName); d.State != core.DrainStateDraining || d.FinishedAt != nil {
		t.Errorf("after start: %+v, want draining and unfinished", d)
	}
	if again, err := s.StartDrain(context.Background(), drainBackendName); err != nil || again {
		t.Errorf("second StartDrain = %v, %v; want refused while draining", again, err)
	}
	if admits(t, s, drainBackendName, "while-draining") {
		t.Error("admission accepted a write on a draining backend")
	}
}

// TestDrainRecord_CountsMovedObjects verifies moved objects add up while the
// drain is in progress and stop counting once it has failed.
func TestDrainRecord_CountsMovedObjects(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	mustStartDrain(t, s, drainBackendName)

	for range 2 {
		if err := s.AddDrainedObjects(ctx, drainBackendName, 3); err != nil {
			t.Fatalf("AddDrainedObjects: %v", err)
		}
	}
	mustFailDrain(t, s, drainBackendName, "list failed")
	if err := s.AddDrainedObjects(ctx, drainBackendName, 1); err != nil {
		t.Fatalf("AddDrainedObjects after failure: %v", err)
	}
	if d := drainFor(t, s, drainBackendName); d.ObjectsMoved != 6 {
		t.Errorf("objects moved = %d, want 6", d.ObjectsMoved)
	}
}

// TestDrainRecord_FailureKeepsRefusing verifies a failed drain records its
// reason and still refuses writes to its backend.
func TestDrainRecord_FailureKeepsRefusing(t *testing.T) {
	s := newTestStore(t)
	mustStartDrain(t, s, drainBackendName)
	mustFailDrain(t, s, drainBackendName, "list failed")

	if d := drainFor(t, s, drainBackendName); d.State != core.DrainStateFailed || d.LastError != "list failed" || d.FinishedAt == nil {
		t.Errorf("after failure: %+v, want failed with its reason and finished", d)
	}
	if admits(t, s, drainBackendName, "while-failed") {
		t.Error("admission accepted a write on a backend whose drain failed")
	}
}

// TestDrainRecord_RestartAfterFailure verifies starting a failed drain again
// resets its record to a fresh drain in progress.
func TestDrainRecord_RestartAfterFailure(t *testing.T) {
	s := newTestStore(t)
	mustStartDrain(t, s, drainBackendName)
	if err := s.AddDrainedObjects(context.Background(), drainBackendName, 3); err != nil {
		t.Fatalf("AddDrainedObjects: %v", err)
	}
	mustFailDrain(t, s, drainBackendName, "list failed")
	mustStartDrain(t, s, drainBackendName)

	if d := drainFor(t, s, drainBackendName); d.State != core.DrainStateDraining || d.ObjectsMoved != 0 || d.LastError != "" || d.FinishedAt != nil {
		t.Errorf("after restart: %+v, want a fresh draining record", d)
	}
}

// TestDrainRecord_ClearReopensTheBackend verifies clearing the record makes the
// backend writable again, and that a second clear finds nothing.
func TestDrainRecord_ClearReopensTheBackend(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	mustStartDrain(t, s, drainBackendName)

	if cleared, err := s.ClearDrain(ctx, drainBackendName); err != nil || !cleared {
		t.Fatalf("ClearDrain = %v, %v; want cleared", cleared, err)
	}
	if cleared, err := s.ClearDrain(ctx, drainBackendName); err != nil || cleared {
		t.Errorf("second ClearDrain = %v, %v; want nothing to clear", cleared, err)
	}
	if !admits(t, s, drainBackendName, "after-clear") {
		t.Error("admission refused a backend whose drain record was cleared")
	}
}

// TestDrainRecord_RefusesReplicaTarget verifies the replica insert, the other
// admission path, refuses a draining target.
func TestDrainRecord_RefusesReplicaTarget(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const key = "bkt/replica"
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key: key, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 1,
	}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	mustStartDrain(t, s, "backend-b")
	if _, inserted, err := s.RecordReplica(ctx, key, "backend-b", "backend-a"); err != nil || inserted {
		t.Errorf("RecordReplica onto a draining backend = %v, %v; want refused", inserted, err)
	}
}

// TestDrainRecord_RefusesMultipartUpload verifies the upload-row insert refuses
// a draining backend.
func TestDrainRecord_RefusesMultipartUpload(t *testing.T) {
	s := newTestStore(t)
	mustStartDrain(t, s, drainBackendName)
	created, err := s.CreateMultipartUpload(context.Background(), &core.CreateMultipartUploadParams{
		UploadID: "upload-1", ObjectKey: "bkt/mp", BackendName: drainBackendName,
	})
	if err != nil || created {
		t.Errorf("CreateMultipartUpload on a draining backend = %v, %v; want refused", created, err)
	}
}

// TestDrainRecord_OtherBackendsStillAdmit verifies a drain refuses only its own
// backend.
func TestDrainRecord_OtherBackendsStillAdmit(t *testing.T) {
	s := newTestStore(t)
	mustStartDrain(t, s, "backend-a")
	if !admits(t, s, "backend-b", "other") {
		t.Error("draining backend-a refused a write on backend-b")
	}
}

// TestCompleteDrain_WaitsForAnObjectRow verifies a drain does not complete while
// an object row remains on the backend, and does once it is gone.
func TestCompleteDrain_WaitsForAnObjectRow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key: "bkt/left", Copies: []core.ObjectCopy{{Backend: drainBackendName}}, Size: 1,
	}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	mustStartDrain(t, s, drainBackendName)
	if completes(t, s, drainBackendName) {
		t.Fatal("completed with an object row still on the backend")
	}
	if _, _, err := s.DeleteObject(ctx, "bkt/left"); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if !completes(t, s, drainBackendName) {
		t.Fatal("did not complete once the backend was empty")
	}
	if d := drainFor(t, s, drainBackendName); d.State != core.DrainStateDrained || d.FinishedAt == nil {
		t.Errorf("after completion: %+v, want drained and finished", d)
	}
}

// TestCompleteDrain_IgnoresUnmanagedRows verifies an object reconcile found
// outside every bucket prefix, which a drain does not move, does not keep the
// drain from finishing.
func TestCompleteDrain_IgnoresUnmanagedRows(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key: "stray/key", Copies: []core.ObjectCopy{{Backend: drainBackendName}}, Size: 1,
	}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE object_locations SET managed = 0 WHERE object_key = 'stray/key'`); err != nil {
		t.Fatalf("mark unmanaged: %v", err)
	}
	mustStartDrain(t, s, drainBackendName)
	if !completes(t, s, drainBackendName) {
		t.Fatal("an unmanaged row kept the drain from completing")
	}
}

// TestCompleteDrain_WaitsForAnUploadAdmittedBefore verifies a drain does not
// complete while a write admitted before it started is still uploading.
func TestCompleteDrain_WaitsForAnUploadAdmittedBefore(t *testing.T) {
	s := newTestStore(t)
	if !admits(t, s, drainBackendName, "in-flight") {
		t.Fatal("admission refused before the drain started")
	}
	mustStartDrain(t, s, drainBackendName)
	if completes(t, s, drainBackendName) {
		t.Fatal("completed while a write admitted before the drain was still uploading")
	}
	if err := s.DeletePending(context.Background(), "in-flight"); err != nil {
		t.Fatalf("DeletePending: %v", err)
	}
	if !completes(t, s, drainBackendName) {
		t.Fatal("did not complete once the upload resolved")
	}
}

// TestCompleteDrain_WaitsForAMultipartUpload verifies a drain does not complete
// while a multipart upload is open on the backend.
func TestCompleteDrain_WaitsForAMultipartUpload(t *testing.T) {
	s := newTestStore(t)
	if created, err := s.CreateMultipartUpload(context.Background(), &core.CreateMultipartUploadParams{
		UploadID: "upload-1", ObjectKey: "bkt/mp", BackendName: drainBackendName,
	}); err != nil || !created {
		t.Fatalf("CreateMultipartUpload = %v, %v; want created before the drain", created, err)
	}
	mustStartDrain(t, s, drainBackendName)
	if completes(t, s, drainBackendName) {
		t.Fatal("completed with a multipart upload still open on the backend")
	}
}

// TestCompleteDrain_OnlyADrainInProgress verifies a failed drain is not
// completed, even with nothing left on the backend.
func TestCompleteDrain_OnlyADrainInProgress(t *testing.T) {
	s := newTestStore(t)
	mustStartDrain(t, s, drainBackendName)
	mustFailDrain(t, s, drainBackendName, "stopped")
	if completes(t, s, drainBackendName) {
		t.Fatal("completed a drain that had failed")
	}
}
