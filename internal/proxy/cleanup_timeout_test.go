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

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// TestUploadPart_RecordFailure_CleanupDeleteCarriesDeadline verifies the upload part record failure cleanup delete carries deadline path by exercising errors.New, context.Background, bytes.NewReader.
func TestUploadPart_RecordFailure_CleanupDeleteCarriesDeadline(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	store := &mockStore{
		getMultipartResp: &core.MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
		},
		recordPartErr: errors.New("db error"),
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	_, err := mgr.MultipartManager.UploadPart(context.Background(), "upload-1", 1, bytes.NewReader([]byte("data")), 4)
	if err == nil {
		t.Fatal("expected error from RecordPart failure to trigger part cleanup")
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if !backend.lastDeleteHadDdl {
		t.Error("part-cleanup DeleteObject should be invoked with a deadline-bound ctx (deleteWithTimeout)")
	}
}
