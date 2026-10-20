// -------------------------------------------------------------------------------
// Completion Mark Check
//
// Author: Alex Freidah
//
// A multipart completion marks its upload and renews the mark while it reads
// the parts. While the mark is fresh, a second completion cannot take it and a
// part record or an upload delete is refused; a mark nobody renewed can be
// taken over. Both engines run these cases against a real store.
// -------------------------------------------------------------------------------

package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// completionMarkReason labels the cleanup rows the check's upload delete
// queues.
const completionMarkReason = "completion_mark_test"

// CompletionMarkStore is the store surface CheckCompletionMark drives, which
// both engines satisfy.
type CompletionMarkStore interface {
	core.MultipartStore
	InsertPendingIfFits(ctx context.Context, p *core.PendingObject, accepts core.BackendFlags) (bool, error)
}

// CheckCompletionMark runs the completion mark cases against s, creating
// uploads whose IDs start with uploadID on backend, which must have room for a
// one-part intent.
func CheckCompletionMark(t *testing.T, s CompletionMarkStore, uploadID, backend string) {
	t.Helper()
	t.Run("begin renew end", func(t *testing.T) {
		checkMarkLifecycle(t, s, uploadID+"/lifecycle", backend)
	})
	t.Run("refuses parts and deletes", func(t *testing.T) {
		checkMarkRefuses(t, s, uploadID+"/refuses", backend)
	})
	t.Run("gone upload", func(t *testing.T) {
		checkMarkGoneUpload(t, s, uploadID+"/gone")
	})
}

// freshCutoff is the stale cutoff a caller passes when any mark renewed in the
// last minute still belongs to a running completion.
func freshCutoff() time.Time {
	return time.Now().Add(-time.Minute)
}

// checkMarkLifecycle verifies a fresh mark cannot be taken twice, a stale one
// can, and renewal reports whether the mark is still held.
func checkMarkLifecycle(t *testing.T, s CompletionMarkStore, uploadID, backend string) {
	t.Helper()
	ctx := context.Background()
	createMarkUpload(t, s, uploadID, backend)

	if ok, err := s.BeginMultipartCompletion(ctx, uploadID, freshCutoff()); err != nil || !ok {
		t.Fatalf("first Begin = %v, %v; want the mark taken", ok, err)
	}
	if ok, err := s.BeginMultipartCompletion(ctx, uploadID, freshCutoff()); err != nil || ok {
		t.Errorf("second Begin = %v, %v; want refused while the mark is fresh", ok, err)
	}
	if ok, err := s.RenewMultipartCompletion(ctx, uploadID); err != nil || !ok {
		t.Errorf("Renew while marked = %v, %v; want held", ok, err)
	}
	if ok, err := s.BeginMultipartCompletion(ctx, uploadID, time.Now().Add(time.Hour)); err != nil || !ok {
		t.Errorf("Begin over a stale mark = %v, %v; want it taken over", ok, err)
	}
	if err := s.EndMultipartCompletion(ctx, uploadID); err != nil {
		t.Fatalf("End: %v", err)
	}
	if ok, err := s.RenewMultipartCompletion(ctx, uploadID); err != nil || ok {
		t.Errorf("Renew after End = %v, %v; want the mark gone", ok, err)
	}
	if ok, err := s.BeginMultipartCompletion(ctx, uploadID, freshCutoff()); err != nil || !ok {
		t.Errorf("Begin after End = %v, %v; want the mark taken", ok, err)
	}
	if _, err := s.DeleteMultipartUpload(ctx, uploadID, completionMarkReason, time.Now().Add(time.Hour)); err != nil {
		t.Errorf("DeleteMultipartUpload past a stale mark: %v", err)
	}
}

// checkMarkGoneUpload verifies an upload that does not exist can be neither
// marked nor renewed, and clearing its mark is not an error.
func checkMarkGoneUpload(t *testing.T, s CompletionMarkStore, uploadID string) {
	t.Helper()
	ctx := context.Background()
	if ok, err := s.BeginMultipartCompletion(ctx, uploadID, freshCutoff()); err != nil || ok {
		t.Errorf("Begin of a gone upload = %v, %v; want false", ok, err)
	}
	if ok, err := s.RenewMultipartCompletion(ctx, uploadID); err != nil || ok {
		t.Errorf("Renew of a gone upload = %v, %v; want false", ok, err)
	}
	if err := s.EndMultipartCompletion(ctx, uploadID); err != nil {
		t.Errorf("End of a gone upload: %v", err)
	}
}

// checkMarkRefuses verifies a part record and an upload delete are refused
// with ErrCompletionInProgress while the mark is fresh, leave the part and the
// upload untouched, and go through once the mark is cleared.
func checkMarkRefuses(t *testing.T, s CompletionMarkStore, uploadID, backend string) {
	t.Helper()
	ctx := context.Background()
	createMarkUpload(t, s, uploadID, backend)
	part := claimMarkPart(t, s, uploadID, backend)

	if ok, err := s.BeginMultipartCompletion(ctx, uploadID, freshCutoff()); err != nil || !ok {
		t.Fatalf("Begin = %v, %v; want the mark taken", ok, err)
	}
	part.CompletionStaleBefore = freshCutoff()
	if _, err := s.RecordPart(ctx, part); !errors.Is(err, core.ErrCompletionInProgress) {
		t.Errorf("RecordPart while marked = %v, want ErrCompletionInProgress", err)
	}
	if parts, err := s.GetParts(ctx, uploadID); err != nil || len(parts) != 0 {
		t.Errorf("parts while marked = %+v, %v; want the refused part unrecorded", parts, err)
	}
	if _, err := s.DeleteMultipartUpload(ctx, uploadID, completionMarkReason, freshCutoff()); !errors.Is(err, core.ErrCompletionInProgress) {
		t.Errorf("DeleteMultipartUpload while marked = %v, want ErrCompletionInProgress", err)
	}
	if _, err := s.GetMultipartUpload(ctx, uploadID); err != nil {
		t.Errorf("GetMultipartUpload after a refused delete: %v", err)
	}

	if err := s.EndMultipartCompletion(ctx, uploadID); err != nil {
		t.Fatalf("End: %v", err)
	}
	part.CompletionStaleBefore = freshCutoff()
	if _, err := s.RecordPart(ctx, part); err != nil {
		t.Fatalf("RecordPart after End: %v", err)
	}
	if parts, err := s.GetParts(ctx, uploadID); err != nil || len(parts) != 1 {
		t.Errorf("parts after End = %+v, %v; want the one part", parts, err)
	}
	cleanup, err := s.DeleteMultipartUpload(ctx, uploadID, completionMarkReason, freshCutoff())
	if err != nil || len(cleanup) != 1 || cleanup[0].StorageKey != part.Intent.StorageKey {
		t.Errorf("DeleteMultipartUpload after End = %+v, %v; want the part queued", cleanup, err)
	}
	if _, err := s.GetMultipartUpload(ctx, uploadID); !errors.Is(err, core.ErrMultipartUploadNotFound) {
		t.Errorf("GetMultipartUpload after delete = %v, want ErrMultipartUploadNotFound", err)
	}
}

// createMarkUpload creates an upload on backend, failing the test unless the
// store took it.
func createMarkUpload(t *testing.T, s CompletionMarkStore, uploadID, backend string) {
	t.Helper()
	created, err := s.CreateMultipartUpload(context.Background(), &core.CreateMultipartUploadParams{
		UploadID: uploadID, ObjectKey: uploadID, BackendName: backend,
	})
	if err != nil || !created {
		t.Fatalf("CreateMultipartUpload(%s) = %v, %v", uploadID, created, err)
	}
}

// claimMarkPart claims the intent for part 1 of the upload, the way an
// UploadPart claims before writing, and returns the record for that part.
func claimMarkPart(t *testing.T, s CompletionMarkStore, uploadID, backend string) *core.RecordPartParams {
	t.Helper()
	path := core.PartPath(uploadID, 1)
	intent := &core.PendingObject{
		IntentID: uploadID + "/intent", ObjectKey: path, StorageKey: path + "!mark",
		BackendName: backend, SizeBytes: 16, Role: core.PendingRoleCompanion,
	}
	fits, err := s.InsertPendingIfFits(context.Background(), intent, core.AcceptForPart)
	if err != nil || !fits {
		t.Fatalf("InsertPendingIfFits(%s) = %v, %v", path, fits, err)
	}
	return &core.RecordPartParams{UploadID: uploadID, PartNumber: 1, ETag: "mark", SizeBytes: 16, Intent: intent}
}
