// -------------------------------------------------------------------------------
// Cleanup-Path Timeout Tests
//
// Author: Alex Freidah
//
// Verifies that orphan-cleanup deletes after a metadata-record failure go
// through deleteWithTimeout (i.e. the per-backend timeout is honored). Without
// the timeout wiring, a hung backend would block the user's request for the
// full request-context lifetime.
// -------------------------------------------------------------------------------

package proxy

import (
	"bytes"
	"context"
	"errors"
	"testing"

	st "github.com/afreidah/s3-orchestrator/internal/store"
)

func TestPutObject_RecordFailure_CleanupDeleteCarriesDeadline(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	store := &mockStore{
		getBackendResp:  "b1",
		recordObjectErr: errors.New("db write failed"),
	}
	// newTestManager sets BackendTimeout to 30s; the cleanup delete must
	// receive a ctx whose Deadline reflects that, not the bare request ctx.
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	_, err := mgr.ObjectManager.PutObject(context.Background(), "k", bytes.NewReader([]byte("data")), 4, "", nil)
	if err == nil {
		t.Fatal("expected error from RecordObject failure to trigger cleanup")
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if !backend.lastDeleteHadDdl {
		t.Error("orphan-cleanup DeleteObject should be invoked with a deadline-bound ctx (deleteWithTimeout)")
	}
}

func TestUploadPart_RecordFailure_CleanupDeleteCarriesDeadline(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	store := &mockStore{
		getMultipartResp: &st.MultipartUpload{
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
