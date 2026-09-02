// -------------------------------------------------------------------------------
// Multipart - Part ETag Consistency Tests
//
// Author: Alex Freidah
//
// UploadPart hands the client the MD5 of the bytes it sent, and completion
// validates the manifest against that same value. ListParts has to report it
// too: a resumed upload rebuilds its manifest from the part listing, and a
// listing that reported the stored part's backend ETag would hand the client
// values completion then rejects.
//
// The two differ exactly when the stored part is not the client's bytes, which
// is what the encrypted case here stands in for.
// -------------------------------------------------------------------------------

package multipart

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // G501: the S3 ETag algorithm
	"encoding/hex"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"

	"go.uber.org/mock/gomock"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// quotedMD5 is the ETag S3 reports for a part carrying these bytes.
func quotedMD5(b []byte) string {
	sum := md5.Sum(b) //nolint:gosec // G401: see above
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestUploadPart_ReturnsClientMD5 pins the value the client is handed: the
// digest of what it sent, not what the backend replied for the stored part.
func TestUploadPart_ReturnsClientMD5(t *testing.T) {
	t.Parallel()
	body := []byte("part-data")
	be := backendtest.NewInMemory()

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
		Return(&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1"}, nil).
		AnyTimes()

	var recorded *core.RecordPartParams
	store.EXPECT().RecordPart(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, p *core.RecordPartParams) error {
			recorded = p
			return nil
		}).AnyTimes()
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	got, err := mgr.UploadPart(context.Background(), "multi", "key", "upload-1", 1, bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("UploadPart: %v", err)
	}
	if want := quotedMD5(body); got != want {
		t.Errorf("etag = %q, want the MD5 of the uploaded bytes %q", got, want)
	}
	if recorded == nil {
		t.Fatal("no part recorded")
	}
	if recorded.PlaintextETag == "" {
		t.Error("part recorded without a plaintext digest; the composite cannot be built from it")
	}
}

// TestGetParts_ReportsTheETagTheClientWasGiven is the ListParts contract. The
// stored ETag differs from the client's digest here, standing in for a part
// stored as an encryption envelope, and the listing must report the latter.
func TestGetParts_ReportsTheETagTheClientWasGiven(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
		Return(&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1"}, nil).
		AnyTimes()
	store.EXPECT().GetParts(gomock.Any(), "upload-1").
		Return([]core.MultipartPart{
			{PartNumber: 1, ETag: `"stored-envelope-etag"`, PlaintextETag: "abc123", SizeBytes: 9},
			{PartNumber: 2, ETag: `"legacy-part-etag"`, SizeBytes: 9},
		}, nil).AnyTimes()
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	parts, err := mgr.GetParts(context.Background(), "multi", "key", "upload-1")
	if err != nil {
		t.Fatalf("GetParts: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2", len(parts))
	}
	if parts[0].ETag != `"abc123"` {
		t.Errorf("part 1 etag = %q, want the client's digest", parts[0].ETag)
	}
	// A part uploaded before per-part digests has none to report, so it keeps
	// the backend value - which is what that client was given at upload.
	if parts[1].ETag != `"legacy-part-etag"` {
		t.Errorf("part 2 etag = %q, want the stored value kept for a pre-digest part", parts[1].ETag)
	}
}

// TestCompleteMultipartUpload_AcceptsTheListedETags closes the loop: the
// manifest a client builds from GetParts is the manifest completion accepts.
func TestCompleteMultipartUpload_AcceptsTheListedETags(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	body := []byte("AAA")
	be := backendtest.NewInMemory()
	_, _ = be.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader(body), int64(len(body)), "application/octet-stream", nil)

	digest := hex.EncodeToString(func() []byte { s := md5.Sum(body); return s[:] }()) //nolint:gosec // G401: see above
	store, _ := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1"},
		[]core.MultipartPart{
			{PartNumber: 1, ETag: `"stored-envelope-etag"`, PlaintextETag: digest, SizeBytes: int64(len(body))},
		}, nil)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	listed, err := mgr.GetParts(ctx, "multi", "key", "upload-1")
	if err != nil {
		t.Fatalf("GetParts: %v", err)
	}

	manifest := make([]core.CompletePart, len(listed))
	for i := range listed {
		manifest[i] = core.CompletePart{PartNumber: listed[i].PartNumber, ETag: listed[i].ETag}
	}

	if _, err := mgr.CompleteMultipartUpload(ctx, "multi", "key", "upload-1", manifest); err != nil {
		t.Fatalf("CompleteMultipartUpload with a manifest built from GetParts: %v", err)
	}
}
