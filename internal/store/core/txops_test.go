// -------------------------------------------------------------------------------
// Shared Transactional Store Method Tests
//
// Author: Alex Freidah
//
// TxOps only binds the package's transactional functions to the Runner its
// engine store supplies. These tests run each method against a stub
// transaction and check it reaches the function it names, which shows in the
// outcome that function gives for that stub.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"errors"
	"testing"
	"time"
)

// noopTxOps binds TxOps to a transaction that holds nothing, so no upload
// exists, and claims every intent.
func noopTxOps() TxOps {
	return NewTxOps(&stubRunner{tx: &noopTxAdapter{}})
}

// TestTxOps_ObjectMethods verifies the object and tag methods run their
// transaction against the bound runner and pass the result through.
func TestTxOps_ObjectMethods(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ops := noopTxOps()

	if _, _, err := ops.RecordObject(ctx, &RecordObjectRequest{Key: "k", Copies: []ObjectCopy{{Backend: "b1"}}}); err != nil {
		t.Errorf("RecordObject: %v", err)
	}
	if _, _, err := ops.DeleteObject(ctx, "k"); !errors.Is(err, ErrObjectNotFound) {
		t.Errorf("DeleteObject err = %v, want ErrObjectNotFound", err)
	}
	if _, _, err := ops.DeleteObjectsBatch(ctx, []string{"k"}); err != nil {
		t.Errorf("DeleteObjectsBatch: %v", err)
	}
	if _, err := ops.DeleteObjectLocation(ctx, &ObjectLocation{ObjectKey: "k", BackendName: "b1"}, 0, ""); !errors.Is(err, ErrCopyChanged) {
		t.Errorf("DeleteObjectLocation err = %v, want ErrCopyChanged", err)
	}
	if err := ops.ReplaceObjectTags(ctx, "k", nil); !errors.Is(err, ErrObjectNotFound) {
		t.Errorf("ReplaceObjectTags err = %v, want ErrObjectNotFound", err)
	}
	if err := ops.DeleteObjectTags(ctx, "k"); !errors.Is(err, ErrObjectNotFound) {
		t.Errorf("DeleteObjectTags err = %v, want ErrObjectNotFound", err)
	}
	if got, err := ops.ImportObject(ctx, &ImportObjectRequest{Key: "k", Backend: "b1"}); err != nil || got != ImportSkippedExisting {
		t.Errorf("ImportObject = %v, %v; want skipped_existing", got, err)
	}
}

// TestTxOps_IntentMethods verifies the methods that resolve an intent or a
// cleanup row run their transaction against the bound runner.
func TestTxOps_IntentMethods(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ops := noopTxOps()
	intent := &PendingObject{IntentID: "i", ObjectKey: "k", BackendName: "b2"}
	if _, _, err := ops.PlaceCopy(ctx, &Placement{Source: &ObjectLocation{ObjectKey: "k", BackendName: "b1"}, Intent: intent}); !errors.Is(err, ErrCopyChanged) {
		t.Errorf("PlaceCopy err = %v, want ErrCopyChanged", err)
	}
	if got, err := ops.DiscardPending(ctx, intent, "r"); err != nil || len(got) != 1 {
		t.Errorf("DiscardPending = %+v, %v; want one queued row", got, err)
	}
	if _, err := ops.RecordPart(ctx, &RecordPartParams{UploadID: "u", PartNumber: 1, Intent: intent}); !errors.Is(err, ErrMultipartUploadNotFound) {
		t.Errorf("RecordPart err = %v, want ErrMultipartUploadNotFound", err)
	}
	if got, err := ops.DeleteMultipartUpload(ctx, "u", "r", time.Now()); err != nil || len(got) != 0 {
		t.Errorf("DeleteMultipartUpload = %+v, %v; want nothing queued", got, err)
	}
}

// TestTxOps_ResolutionMethods verifies the reaper and housekeeping methods run
// their transaction against the bound runner.
func TestTxOps_ResolutionMethods(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ops := noopTxOps()
	intent := &PendingObject{IntentID: "i", ObjectKey: "k", BackendName: "b2"}
	if fits, err := ops.InsertPendingIfFits(ctx, intent, 0); err != nil || !fits {
		t.Errorf("InsertPendingIfFits = %v, %v; want fits", fits, err)
	}
	if got, _, _, err := ops.PromotePending(ctx, intent); err != nil || got != PendingPromoteCommitted {
		t.Errorf("PromotePending = %v, %v; want committed", got, err)
	}
	if got, _, _, err := ops.CommitCompanionCopy(ctx, intent); err != nil || got != CompanionCopyCommitted {
		t.Errorf("CommitCompanionCopy = %v, %v; want committed", got, err)
	}
}
