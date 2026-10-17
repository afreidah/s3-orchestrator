// -------------------------------------------------------------------------------
// Backend State Record Integration Tests
//
// Author: Alex Freidah
//
// Drives a drain through the backend state record against a real PostgreSQL and checks
// that admission refuses the backend for as long as a record of any state
// exists. The database is shared across tests, so each test drains a backend of
// its own.
// -------------------------------------------------------------------------------

//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// drainBackend registers a backend unique to the test and clears its drain
// record when the test ends.
func drainBackend(t *testing.T, s *Store) string {
	t.Helper()
	ctx := context.Background()
	name := uniqueKey(t, "backend")
	if err := s.SyncQuotaLimits(ctx, []config.BackendConfig{{Name: name, QuotaBytes: 1 << 30}}); err != nil {
		t.Fatalf("SyncQuotaLimits: %v", err)
	}
	t.Cleanup(func() { _, _ = s.ClearBackendState(context.Background(), name) })
	return name
}

// pgDrainFor returns the named backend's drain record, failing the test when
// there is none.
func pgDrainFor(t *testing.T, s *Store, backend string) core.BackendStateRecord {
	t.Helper()
	drains, err := s.ListBackendStates(context.Background())
	if err != nil {
		t.Fatalf("ListBackendStates: %v", err)
	}
	for _, d := range drains {
		if d.BackendName == backend {
			return d
		}
	}
	t.Fatalf("no drain record for %s", backend)
	return core.BackendStateRecord{}
}

// pgAdmits reports whether admission accepts a one-byte write on the backend,
// removing the intent it claimed.
func pgAdmits(t *testing.T, s *Store, backend, intentID string) bool {
	t.Helper()
	ctx := context.Background()
	id := uniqueKey(t, intentID)
	ok, err := s.InsertPendingIfFits(ctx, &core.PendingObject{
		IntentID: id, ObjectKey: id, BackendName: backend, SizeBytes: 1,
	})
	if err != nil {
		t.Fatalf("InsertPendingIfFits: %v", err)
	}
	if ok {
		t.Cleanup(func() { _ = s.DeletePending(context.Background(), id) })
	}
	return ok
}

// pgStartDrain starts a drain of the backend, failing the test unless it
// started.
func pgStartDrain(t *testing.T, s *Store, backend string) {
	t.Helper()
	if started, err := s.StartDrain(context.Background(), backend); err != nil || !started {
		t.Fatalf("StartDrain(%s) = %v, %v; want started", backend, started, err)
	}
}

// pgFailDrain marks the backend's drain failed.
func pgFailDrain(t *testing.T, s *Store, backend, reason string) {
	t.Helper()
	if err := s.MarkDrainFailed(context.Background(), backend, reason); err != nil {
		t.Fatalf("MarkDrainFailed: %v", err)
	}
}

// pgCompletes reports whether CompleteDrain marked the backend drained.
func pgCompletes(t *testing.T, s *Store, backend string) bool {
	t.Helper()
	done, err := s.CompleteDrain(context.Background(), backend)
	if err != nil {
		t.Fatalf("CompleteDrain: %v", err)
	}
	return done
}

// pgRecordOn records a one-byte object with its only copy on the backend and
// removes it when the test ends. Returns the key.
func pgRecordOn(t *testing.T, s *Store, backend, suffix string) string {
	t.Helper()
	key := uniqueKey(t, suffix)
	if _, _, err := s.RecordObject(context.Background(), &core.RecordObjectRequest{
		Key: key, Copies: []core.ObjectCopy{{Backend: backend}}, Size: 1,
	}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	t.Cleanup(func() { _, _, _ = s.DeleteObject(context.Background(), key) })
	return key
}

// TestPgDrainRecord_StartRefusesAdmission verifies a started drain is recorded
// in progress, cannot be started again, and refuses writes to its backend.
func TestPgDrainRecord_StartRefusesAdmission(t *testing.T) {
	s := adapterPgStore(t)
	backend := drainBackend(t, s)
	pgStartDrain(t, s, backend)

	if d := pgDrainFor(t, s, backend); d.Flags != core.FlagDraining || d.FinishedAt != nil {
		t.Errorf("after start: %+v, want draining and unfinished", d)
	}
	if again, err := s.StartDrain(context.Background(), backend); err != nil || again {
		t.Errorf("second StartDrain = %v, %v; want refused while draining", again, err)
	}
	if pgAdmits(t, s, backend, "while-draining") {
		t.Error("admission accepted a write on a draining backend")
	}
}

// TestPgDrainRecord_CountsMovedObjects verifies moved objects add up while the
// drain is in progress and stop counting once it has failed.
func TestPgDrainRecord_CountsMovedObjects(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	backend := drainBackend(t, s)
	pgStartDrain(t, s, backend)

	for range 2 {
		if err := s.AddDrainedObjects(ctx, backend, 3); err != nil {
			t.Fatalf("AddDrainedObjects: %v", err)
		}
	}
	pgFailDrain(t, s, backend, "list failed")
	if err := s.AddDrainedObjects(ctx, backend, 1); err != nil {
		t.Fatalf("AddDrainedObjects after failure: %v", err)
	}
	if d := pgDrainFor(t, s, backend); d.ObjectsMoved != 6 {
		t.Errorf("objects moved = %d, want 6", d.ObjectsMoved)
	}
}

// TestPgDrainRecord_FailureKeepsRefusing verifies a failed drain records its
// reason and still refuses writes to its backend.
func TestPgDrainRecord_FailureKeepsRefusing(t *testing.T) {
	s := adapterPgStore(t)
	backend := drainBackend(t, s)
	pgStartDrain(t, s, backend)
	pgFailDrain(t, s, backend, "list failed")

	if d := pgDrainFor(t, s, backend); d.Flags != core.FlagDrainFailed || d.LastError != "list failed" || d.FinishedAt == nil {
		t.Errorf("after failure: %+v, want failed with its reason and finished", d)
	}
	if pgAdmits(t, s, backend, "while-failed") {
		t.Error("admission accepted a write on a backend whose drain failed")
	}
}

// TestPgDrainRecord_RestartAfterFailure verifies starting a failed drain again
// resets its record to a fresh drain in progress.
func TestPgDrainRecord_RestartAfterFailure(t *testing.T) {
	s := adapterPgStore(t)
	backend := drainBackend(t, s)
	pgStartDrain(t, s, backend)
	if err := s.AddDrainedObjects(context.Background(), backend, 3); err != nil {
		t.Fatalf("AddDrainedObjects: %v", err)
	}
	pgFailDrain(t, s, backend, "list failed")
	pgStartDrain(t, s, backend)

	if d := pgDrainFor(t, s, backend); d.Flags != core.FlagDraining || d.ObjectsMoved != 0 || d.LastError != "" || d.FinishedAt != nil {
		t.Errorf("after restart: %+v, want a fresh draining record", d)
	}
}

// TestPgDrainRecord_ClearReopensTheBackend verifies clearing the record makes
// the backend writable again, and that a second clear finds nothing.
func TestPgDrainRecord_ClearReopensTheBackend(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	backend := drainBackend(t, s)
	pgStartDrain(t, s, backend)

	if cleared, err := s.ClearBackendState(ctx, backend); err != nil || !cleared {
		t.Fatalf("ClearBackendState = %v, %v; want cleared", cleared, err)
	}
	if cleared, err := s.ClearBackendState(ctx, backend); err != nil || cleared {
		t.Errorf("second ClearBackendState = %v, %v; want nothing to clear", cleared, err)
	}
	if !pgAdmits(t, s, backend, "after-clear") {
		t.Error("admission refused a backend whose drain record was cleared")
	}
}

// TestPgDrainRecord_RefusesReplicaTarget verifies a placement, the other
// admission path, refuses a draining target and records nothing there.
func TestPgDrainRecord_RefusesReplicaTarget(t *testing.T) {
	s := adapterPgStore(t)
	target := drainBackend(t, s)
	key := pgRecordOn(t, s, "backend-a", "k")
	pgStartDrain(t, s, target)
	if _, err := s.PlaceCopy(context.Background(), replicaOf(t, s, key, target, "backend-a")); !errors.Is(err, core.ErrTargetRefused) {
		t.Errorf("PlaceCopy onto a draining backend = %v; want ErrTargetRefused", err)
	}
	if locs, err := s.GetAllObjectLocations(context.Background(), key); err != nil || len(locs) != 1 {
		t.Errorf("copies after a refused placement = %+v, %v; want only the source", locs, err)
	}
}

// TestPgDrainRecord_RefusesMultipartUpload verifies the upload-row insert
// refuses a draining backend.
func TestPgDrainRecord_RefusesMultipartUpload(t *testing.T) {
	s := adapterPgStore(t)
	backend := drainBackend(t, s)
	pgStartDrain(t, s, backend)
	created, err := s.CreateMultipartUpload(context.Background(), &core.CreateMultipartUploadParams{
		UploadID: uniqueKey(t, "upload"), ObjectKey: uniqueKey(t, "mp"), BackendName: backend,
	})
	if err != nil || created {
		t.Errorf("CreateMultipartUpload on a draining backend = %v, %v; want refused", created, err)
	}
}

// TestPgCompleteDrain_WaitsForAnObjectRow verifies a drain does not complete
// while an object row remains on the backend, and does once it is gone.
func TestPgCompleteDrain_WaitsForAnObjectRow(t *testing.T) {
	s := adapterPgStore(t)
	backend := drainBackend(t, s)
	key := pgRecordOn(t, s, backend, "left")
	pgStartDrain(t, s, backend)

	if pgCompletes(t, s, backend) {
		t.Fatal("completed with an object row still on the backend")
	}
	if _, _, err := s.DeleteObject(context.Background(), key); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if !pgCompletes(t, s, backend) {
		t.Fatal("did not complete once the backend was empty")
	}
	if d := pgDrainFor(t, s, backend); d.Flags != core.FlagDrained || d.FinishedAt == nil {
		t.Errorf("after completion: %+v, want drained and finished", d)
	}
}

// TestPgCompleteDrain_IgnoresUnmanagedRows verifies an object reconcile found
// outside every bucket prefix, which a drain does not move, does not keep the
// drain from finishing.
func TestPgCompleteDrain_IgnoresUnmanagedRows(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	backend := drainBackend(t, s)
	key := pgRecordOn(t, s, backend, "stray")
	if _, err := s.pool.Exec(ctx, `UPDATE object_locations SET managed = false WHERE object_key = $1`, key); err != nil {
		t.Fatalf("mark unmanaged: %v", err)
	}
	pgStartDrain(t, s, backend)
	if !pgCompletes(t, s, backend) {
		t.Fatal("an unmanaged row kept the drain from completing")
	}
}

// TestPgCompleteDrain_WaitsForAnUploadAdmittedBefore verifies a drain does not
// complete while a write admitted before it started is still uploading.
func TestPgCompleteDrain_WaitsForAnUploadAdmittedBefore(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	backend := drainBackend(t, s)
	id := uniqueKey(t, "in-flight")
	if ok, err := s.InsertPendingIfFits(ctx, &core.PendingObject{
		IntentID: id, ObjectKey: id, BackendName: backend, SizeBytes: 1,
	}); err != nil || !ok {
		t.Fatalf("InsertPendingIfFits = %v, %v; want admitted before the drain", ok, err)
	}
	pgStartDrain(t, s, backend)

	if pgCompletes(t, s, backend) {
		t.Fatal("completed while a write admitted before the drain was still uploading")
	}
	if err := s.DeletePending(ctx, id); err != nil {
		t.Fatalf("DeletePending: %v", err)
	}
	if !pgCompletes(t, s, backend) {
		t.Fatal("did not complete once the upload resolved")
	}
}

// TestPgCompleteDrain_WaitsForAMultipartUpload verifies a drain does not
// complete while a multipart upload is open on the backend.
func TestPgCompleteDrain_WaitsForAMultipartUpload(t *testing.T) {
	s := adapterPgStore(t)
	backend := drainBackend(t, s)
	uploadID := uniqueKey(t, "upload")
	if created, err := s.CreateMultipartUpload(context.Background(), &core.CreateMultipartUploadParams{
		UploadID: uploadID, ObjectKey: uniqueKey(t, "mp"), BackendName: backend,
	}); err != nil || !created {
		t.Fatalf("CreateMultipartUpload = %v, %v; want created before the drain", created, err)
	}
	t.Cleanup(func() { _ = s.DeleteMultipartUpload(context.Background(), uploadID) })
	pgStartDrain(t, s, backend)

	if pgCompletes(t, s, backend) {
		t.Fatal("completed with a multipart upload still open on the backend")
	}
}

// TestPgCompleteDrain_OnlyADrainInProgress verifies a failed drain is not
// completed, even with nothing left on the backend.
func TestPgCompleteDrain_OnlyADrainInProgress(t *testing.T) {
	s := adapterPgStore(t)
	backend := drainBackend(t, s)
	pgStartDrain(t, s, backend)
	pgFailDrain(t, s, backend, "stopped")
	if pgCompletes(t, s, backend) {
		t.Fatal("completed a drain that had failed")
	}
}

// TestPgBackendFlags_EachRefusesAdmission sets every flag in turn and verifies
// admission refuses the backend while it is set and accepts it once cleared,
// so the mask admission passes to SQL matches what writes accept in Go.
func TestPgBackendFlags_EachRefusesAdmission(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	backend := drainBackend(t, s)
	for i, flag := range []core.BackendFlags{core.FlagCordoned, core.FlagDraining, core.FlagDrained, core.FlagDrainFailed} {
		if set, err := s.SetBackendFlag(ctx, backend, flag); err != nil || !set {
			t.Fatalf("SetBackendFlag(%s) = %v, %v", flag, set, err)
		}
		if pgAdmits(t, s, backend, fmt.Sprintf("flagged-%d", i)) != flag.AcceptedBy(core.AcceptForWrite) {
			t.Errorf("admission with %s disagrees with AcceptForWrite", flag)
		}
		if cleared, err := s.ClearBackendFlags(ctx, backend, flag); err != nil || !cleared {
			t.Fatalf("ClearBackendFlags(%s) = %v, %v", flag, cleared, err)
		}
		if !pgAdmits(t, s, backend, fmt.Sprintf("cleared-%d", i)) {
			t.Errorf("admission refused the backend after %s was cleared", flag)
		}
	}
}

// TestPgBackendFlags_CordonAndDrainAreIndependent verifies a drain keeps a
// cordon, cancelling the drain leaves the cordon, and uncordoning leaves a
// drain.
func TestPgBackendFlags_CordonAndDrainAreIndependent(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	backend := drainBackend(t, s)
	if set, err := s.SetBackendFlag(ctx, backend, core.FlagCordoned); err != nil || !set {
		t.Fatalf("cordon = %v, %v", set, err)
	}
	pgStartDrain(t, s, backend)
	if d := pgDrainFor(t, s, backend); d.Flags != core.FlagCordoned|core.FlagDraining {
		t.Fatalf("after drain on a cordon: %s, want cordoned and draining", d.Flags)
	}
	if _, err := s.ClearBackendFlags(ctx, backend, core.DrainFlags); err != nil {
		t.Fatalf("cancel drain: %v", err)
	}
	if d := pgDrainFor(t, s, backend); d.Flags != core.FlagCordoned {
		t.Errorf("after cancelling the drain: %s, want cordoned only", d.Flags)
	}
	pgStartDrain(t, s, backend)
	if _, err := s.ClearBackendFlags(ctx, backend, core.FlagCordoned); err != nil {
		t.Fatalf("uncordon: %v", err)
	}
	if d := pgDrainFor(t, s, backend); d.Flags != core.FlagDraining {
		t.Errorf("after uncordoning: %s, want draining only", d.Flags)
	}
}
