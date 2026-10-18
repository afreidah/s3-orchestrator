// -------------------------------------------------------------------------------
// Cleanup-Path Timeout Tests
//
// Author: Alex Freidah
//
// Verifies that the delete settling a discarded part carries the per-backend
// timeout. Without it, a hung backend would block the user's request for the
// full request-context lifetime.
// -------------------------------------------------------------------------------

package multipart

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// TestUploadPart_RecordFailure_CleanupDeleteCarriesDeadline asserts the
// delete of a part discarded after a record failure carries a deadline.
func TestUploadPart_RecordFailure_CleanupDeleteCarriesDeadline(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetMultipartUpload(gomock.Any(), "upload-1").
		Return(&core.MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
		}, nil).
		AnyTimes()
	store.EXPECT().RecordPart(gomock.Any(), gomock.Any()).
		Return(nil, errors.New("db error")).
		AnyTimes()
	multipartStubs(t, store)
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	_, err := mgr.UploadPart(context.Background(), "multi", "key", "upload-1", 1, bytes.NewReader([]byte("data")), 4)
	if err == nil {
		t.Fatal("expected error from RecordPart failure to trigger part cleanup")
	}

	if hadDeadline, _ := be.LastDelete(); !hadDeadline {
		t.Error("part-cleanup DeleteObject should be invoked with a deadline-bound ctx")
	}
}
