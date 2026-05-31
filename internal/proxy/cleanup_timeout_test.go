// -------------------------------------------------------------------------------
// Cleanup-Path Timeout Tests
//
// Author: Alex Freidah
//
// Verifies that orphan-cleanup deletes after a metadata-record failure go
// through deleteWithTimeout (i.e. the per-backend timeout is honored). Without
// the timeout wiring, a hung backend would block the user's request for the
// full request-context lifetime.
//
// Note: PutObject no longer goes through the orphan-cleanup path; the
// pending-row pattern leaves bytes on the backend for the reaper to
// resolve. Multipart commit still uses the legacy cleanup-on-failure
// flow until the pending pattern extends to the multipart manager.
// -------------------------------------------------------------------------------

package proxy

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// TestUploadPart_RecordFailure_CleanupDeleteCarriesDeadline asserts the
// upload-part record-failure cleanup delete carries a deadline.
func TestUploadPart_RecordFailure_CleanupDeleteCarriesDeadline(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetMultipartUpload(gomock.Any(), "upload-1").
		Return(&core.MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
		}, nil).
		AnyTimes()
	store.EXPECT().RecordPart(gomock.Any(), "upload-1", 1, gomock.Any(), int64(4), gomock.Any()).
		Return(errors.New("db error")).
		AnyTimes()
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil).
		AnyTimes()

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": backend})

	_, err := mgr.multipartManager.UploadPart(context.Background(), "multi", "key", "upload-1", 1, bytes.NewReader([]byte("data")), 4)
	if err == nil {
		t.Fatal("expected error from RecordPart failure to trigger part cleanup")
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if !backend.lastDeleteHadDdl {
		t.Error("part-cleanup DeleteObject should be invoked with a deadline-bound ctx (deleteWithTimeout)")
	}
}
