// -------------------------------------------------------------------------------
// Part Intent Tests
//
// Author: Alex Freidah
//
// Covers how a part upload claims its backend, where its bytes land, and how
// the cleanup rows its record and the upload's removal queue are settled.
// -------------------------------------------------------------------------------

package multipart

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// partUploadID is the upload every test here writes parts to.
const partUploadID = "upload-1"

// partUploadStore returns a mock store whose upload-1 lives on b1, with the
// default multipart stubs wired after any expectations extra registers.
func partUploadStore(t *testing.T, extra func(*storetest.MockMetadataStore)) (*storetest.MockMetadataStore, *multipartCalls) {
	t.Helper()
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().GetMultipartUpload(gomock.Any(), partUploadID).
		Return(&core.MultipartUpload{UploadID: partUploadID, ObjectKey: "multi/key", BackendName: "b1"}, nil).AnyTimes()
	if extra != nil {
		extra(store)
	}
	c := multipartStubs(t, store)
	storetest.Permissive(store)
	return store, c
}

// uploadPart sends a four-byte part 1 of upload-1.
func uploadPart(mgr *fleet) (string, error) {
	return mgr.UploadPart(context.Background(), "multi", "key", partUploadID, 1, bytes.NewReader([]byte("data")), 4)
}

// TestUploadPart_ClaimsIntentAndWritesItsPath asserts the part claims the
// upload's backend under the part-admission mask before its PUT, writes to the
// intent's own path, and hands the intent to RecordPart.
func TestUploadPart_ClaimsIntentAndWritesItsPath(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	var claimed *core.PendingObject
	store, c := partUploadStore(t, func(s *storetest.MockMetadataStore) {
		s.EXPECT().InsertPendingIfFits(gomock.Any(), gomock.Any(), core.AcceptForPart).
			DoAndReturn(func(_ context.Context, p *core.PendingObject, _ core.BackendFlags) (bool, error) {
				if be.Len() != 0 {
					t.Error("the part was written before its target was claimed")
				}
				claimed = p
				return true, nil
			})
	})
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	if _, err := uploadPart(mgr); err != nil {
		t.Fatalf("UploadPart: %v", err)
	}
	if claimed == nil {
		t.Fatal("no intent was claimed")
	}
	if claimed.ObjectKey != core.PartPath(partUploadID, 1) || claimed.BackendName != "b1" ||
		claimed.Role != core.PendingRoleCompanion || claimed.SizeBytes != 4 {
		t.Errorf("intent = %+v, want a companion for part 1 on b1 of 4 bytes", claimed)
	}
	if !be.Has(claimed.StorageKey) || !strings.HasPrefix(claimed.StorageKey, core.PartPath(partUploadID, 1)) {
		t.Errorf("part not stored at the intent path %q", claimed.StorageKey)
	}
	if len(c.recordPart) != 1 || c.recordPart[0].storageKey != claimed.StorageKey {
		t.Errorf("RecordPart calls = %+v, want one carrying the intent", c.recordPart)
	}
}

// TestUploadPart_ClaimRefused_InsufficientStorage asserts a backend that
// declines the part answers ErrInsufficientStorage and nothing is written.
func TestUploadPart_ClaimRefused_InsufficientStorage(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	store, c := partUploadStore(t, func(s *storetest.MockMetadataStore) {
		s.EXPECT().InsertPendingIfFits(gomock.Any(), gomock.Any(), gomock.Any()).Return(false, nil)
	})
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	if _, err := uploadPart(mgr); !errors.Is(err, core.ErrInsufficientStorage) {
		t.Fatalf("err = %v, want ErrInsufficientStorage", err)
	}
	if be.Len() != 0 || len(c.recordPart) != 0 {
		t.Errorf("objects = %d, records = %d; want nothing written or recorded", be.Len(), len(c.recordPart))
	}
}

// TestUploadPart_ClaimError_Propagates asserts a database failure on the claim
// surfaces as itself rather than as a storage refusal.
func TestUploadPart_ClaimError_Propagates(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	want := errors.New("claim boom")
	store, _ := partUploadStore(t, func(s *storetest.MockMetadataStore) {
		s.EXPECT().InsertPendingIfFits(gomock.Any(), gomock.Any(), gomock.Any()).Return(false, want)
	})
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	_, err := uploadPart(mgr)
	if !errors.Is(err, want) || errors.Is(err, core.ErrInsufficientStorage) {
		t.Fatalf("err = %v, want the claim error", err)
	}
	if be.Len() != 0 {
		t.Error("nothing should be written when the claim fails")
	}
}

// TestUploadPart_ReplacedPartIsSettled asserts the cleanup row RecordPart
// queues for the part it replaced is settled, deleting the old bytes.
func TestUploadPart_ReplacedPartIsSettled(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	old := core.PartPath(partUploadID, 1) + "!old"
	be.Put(old, &backendtest.Object{Data: []byte("prev")})
	replaced := []core.CleanupItem{{
		ID: 99, BackendName: "b1", ObjectKey: core.PartPath(partUploadID, 1), StorageKey: old,
		Reason: core.CleanupReasonPartReplaced, SizeBytes: 4,
	}}
	store, c := partUploadStore(t, func(s *storetest.MockMetadataStore) {
		s.EXPECT().RecordPart(gomock.Any(), gomock.Any()).Return(replaced, nil)
	})
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	if _, err := uploadPart(mgr); err != nil {
		t.Fatalf("UploadPart: %v", err)
	}
	if be.Has(old) {
		t.Error("the replaced part's bytes should be deleted")
	}
	if !be.HasCopyOf(core.PartPath(partUploadID, 1)) {
		t.Error("the new part's bytes must stay")
	}
	if len(c.discarded) != 0 {
		t.Errorf("discarded = %+v, want none on success", c.discarded)
	}
}

// TestCompleteMultipartUpload_ReadsPartsAtTheirPaths asserts assembly reads
// each part from the path its row names rather than the bare part path.
func TestCompleteMultipartUpload_ReadsPartsAtTheirPaths(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	parts := []core.MultipartPart{
		{PartNumber: 1, SizeBytes: 3, StorageKey: core.PartPath(partUploadID, 1) + "!a"},
		{PartNumber: 2, SizeBytes: 3, StorageKey: core.PartPath(partUploadID, 2) + "!b"},
	}
	be.Put(parts[0].Path(partUploadID), &backendtest.Object{Data: []byte("AAA")})
	be.Put(parts[1].Path(partUploadID), &backendtest.Object{Data: []byte("BBB")})
	store, c := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: partUploadID, ObjectKey: "multi/key", BackendName: "b1", ContentType: "application/zip"},
		parts, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	if _, err := mgr.CompleteMultipartUpload(context.Background(), "multi", "key", partUploadID, partsOf(1, 2)); err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}
	obj, ok := be.CopyOf("multi/key")
	if !ok || string(obj.Data) != "AAABBB" {
		t.Errorf("assembled = %q, %v; want AAABBB", obj.Data, ok)
	}
	for _, p := range parts {
		if be.Has(p.Path(partUploadID)) {
			t.Errorf("part %d still on the backend after completion", p.PartNumber)
		}
	}
	if len(c.deleteReasons) != 1 || c.deleteReasons[0] != reasonCompletePart {
		t.Errorf("delete reasons = %v, want [%s]", c.deleteReasons, reasonCompletePart)
	}
}

// TestCompleteMultipartUpload_UploadRemovalFails_KeepsObject asserts a failed
// removal of the completed upload does not fail the completion and deletes
// nothing, leaving the parts to the stale-upload sweep.
func TestCompleteMultipartUpload_UploadRemovalFails_KeepsObject(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	parts := putParts(t, be, 1)
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
		Return(&core.MultipartUpload{UploadID: partUploadID, ObjectKey: "multi/key", BackendName: "b1", ContentType: "application/zip"}, nil).AnyTimes()
	store.EXPECT().GetParts(gomock.Any(), gomock.Any()).Return(parts, nil).AnyTimes()
	store.EXPECT().DeleteMultipartUpload(gomock.Any(), partUploadID, reasonCompletePart, gomock.Any()).
		Return(nil, errors.New("db error"))
	multipartStubs(t, store)
	storetest.Permissive(store)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	if _, err := mgr.CompleteMultipartUpload(context.Background(), "multi", "key", partUploadID, partsOf(1)); err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}
	if !be.Has(parts[0].Path(partUploadID)) {
		t.Error("part bytes must stay when the upload row could not be removed")
	}
}
