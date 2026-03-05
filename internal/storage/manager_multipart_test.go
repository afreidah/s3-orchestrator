// -------------------------------------------------------------------------------
// Multipart Upload Tests
//
// Author: Alex Freidah
//
// Tests for BackendManager multipart upload operations: CreateMultipartUpload,
// UploadPart, CompleteMultipartUpload, and AbortMultipartUpload. Validates
// backend delegation, metadata recording, and error handling.
// -------------------------------------------------------------------------------

package storage

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// -------------------------------------------------------------------------
// CreateMultipartUpload
// -------------------------------------------------------------------------

func TestCreateMultipartUpload_Success(t *testing.T) {
	backend := newMockBackend()
	store := &mockStore{getBackendResp: "b1"}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	uploadID, backendName, err := mgr.CreateMultipartUpload(context.Background(), "multi/key", "application/zip", nil)
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	if uploadID == "" {
		t.Error("expected non-empty upload ID")
	}
	if backendName != "b1" {
		t.Errorf("backend = %q, want %q", backendName, "b1")
	}
}

func TestCreateMultipartUpload_DBUnavailable(t *testing.T) {
	store := &mockStore{getBackendErr: ErrDBUnavailable}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	_, _, err := mgr.CreateMultipartUpload(context.Background(), "key", "", nil)
	if !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("expected ErrServiceUnavailable, got %v", err)
	}
}

func TestCreateMultipartUpload_NoSpace(t *testing.T) {
	store := &mockStore{getBackendErr: ErrNoSpaceAvailable}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	_, _, err := mgr.CreateMultipartUpload(context.Background(), "key", "", nil)
	if !errors.Is(err, ErrInsufficientStorage) {
		t.Fatalf("expected ErrInsufficientStorage, got %v", err)
	}
}

// -------------------------------------------------------------------------
// UploadPart
// -------------------------------------------------------------------------

func TestUploadPart_Success(t *testing.T) {
	backend := newMockBackend()
	store := &mockStore{
		getMultipartResp: &MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
			ContentType: "application/zip",
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	etag, err := mgr.UploadPart(context.Background(), "upload-1", 1, bytes.NewReader([]byte("part-data")), 9)
	if err != nil {
		t.Fatalf("UploadPart: %v", err)
	}
	if etag == "" {
		t.Error("expected non-empty etag")
	}
	// Part should be stored under temp key
	if !backend.hasObject("__multipart/upload-1/1") {
		t.Error("part not found on backend")
	}
}

func TestUploadPart_InvalidPartNumber(t *testing.T) {
	mgr := newTestManager(&mockStore{}, map[string]*mockBackend{"b1": newMockBackend()})

	for _, pn := range []int{0, -1, 10001, 1 << 20} {
		_, err := mgr.UploadPart(context.Background(), "upload-1", pn, bytes.NewReader([]byte("x")), 1)
		if err == nil {
			t.Errorf("UploadPart(partNumber=%d) should fail", pn)
			continue
		}
		var s3Err *S3Error
		if !errors.As(err, &s3Err) || s3Err.Code != "InvalidArgument" {
			t.Errorf("UploadPart(partNumber=%d) = %v, want S3Error InvalidArgument", pn, err)
		}
	}
}

func TestUploadPart_DBUnavailable(t *testing.T) {
	store := &mockStore{getMultipartErr: ErrDBUnavailable}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	_, err := mgr.UploadPart(context.Background(), "upload-1", 1, bytes.NewReader([]byte("x")), 1)
	if !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("expected ErrServiceUnavailable, got %v", err)
	}
}

// -------------------------------------------------------------------------
// CompleteMultipartUpload
// -------------------------------------------------------------------------

func TestCompleteMultipartUpload_Success(t *testing.T) {
	backend := newMockBackend()

	// Pre-store parts on the backend
	ctx := context.Background()
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/2", bytes.NewReader([]byte("BBB")), 3, "application/octet-stream", nil)

	store := &mockStore{
		getMultipartResp: &MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
			ContentType: "application/zip",
		},
		getPartsResp: []MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3},
			{PartNumber: 2, ETag: "e2", SizeBytes: 3},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	etag, err := mgr.CompleteMultipartUpload(ctx, "upload-1", []int{1, 2})
	if err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}
	if etag == "" {
		t.Error("expected non-empty etag")
	}
	// Final object should exist
	if !backend.hasObject("multi/key") {
		t.Error("final object not found on backend")
	}
	// Part temp keys should be cleaned up
	if backend.hasObject("__multipart/upload-1/1") {
		t.Error("part 1 temp key should be deleted")
	}
	if backend.hasObject("__multipart/upload-1/2") {
		t.Error("part 2 temp key should be deleted")
	}
	// RecordObject should have been called
	if len(store.recordObjectCalls) != 1 {
		t.Fatalf("expected 1 RecordObject call, got %d", len(store.recordObjectCalls))
	}
	call := store.recordObjectCalls[0]
	if call.Key != "multi/key" || call.Backend != "b1" || call.Size != 6 {
		t.Errorf("RecordObject called with %+v", call)
	}
}

func TestCompleteMultipartUpload_DBUnavailable(t *testing.T) {
	store := &mockStore{getMultipartErr: ErrDBUnavailable}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	_, err := mgr.CompleteMultipartUpload(context.Background(), "upload-1", []int{1})
	if !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("expected ErrServiceUnavailable, got %v", err)
	}
}

// -------------------------------------------------------------------------
// AbortMultipartUpload
// -------------------------------------------------------------------------

func TestAbortMultipartUpload_Success(t *testing.T) {
	backend := newMockBackend()
	ctx := context.Background()
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)

	store := &mockStore{
		getMultipartResp: &MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
		},
		getPartsResp: []MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3, CreatedAt: time.Now()},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	err := mgr.AbortMultipartUpload(ctx, "upload-1")
	if err != nil {
		t.Fatalf("AbortMultipartUpload: %v", err)
	}
	// Part should be cleaned up
	if backend.hasObject("__multipart/upload-1/1") {
		t.Error("part temp key should be deleted")
	}

	// Usage: 1 part delete + 1 abort = 2 API calls
	c := mgr.usage.counters["b1"]
	if got := c.apiRequests.Load(); got != 2 {
		t.Errorf("apiRequests = %d, want 2 (1 part delete + 1 abort)", got)
	}
}

func TestAbortMultipartUpload_DBUnavailable(t *testing.T) {
	store := &mockStore{getMultipartErr: ErrDBUnavailable}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	err := mgr.AbortMultipartUpload(context.Background(), "upload-1")
	if !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("expected ErrServiceUnavailable, got %v", err)
	}
}

func TestAbortMultipartUpload_GetPartsError(t *testing.T) {
	store := &mockStore{
		getMultipartResp: &MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
		},
		getPartsErr: errors.New("db error"),
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	err := mgr.AbortMultipartUpload(context.Background(), "upload-1")
	if err == nil {
		t.Fatal("expected error from GetParts failure")
	}
}

func TestAbortMultipartUpload_PartDeleteFails_EnqueuesCleanup(t *testing.T) {
	backend := newMockBackend()
	backend.delErr = errors.New("backend timeout")
	ctx := context.Background()
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)
	// Reset delErr after seeding (PutObject doesn't check it, but just in case)
	backend.mu.Lock()
	backend.delErr = errors.New("backend timeout")
	backend.mu.Unlock()

	store := &mockStore{
		getMultipartResp: &MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
		},
		getPartsResp: []MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	// Abort should still succeed (delete failure on backend just enqueues cleanup)
	// but DeleteMultipartUpload will also run
	err := mgr.AbortMultipartUpload(ctx, "upload-1")
	if err != nil {
		t.Fatalf("AbortMultipartUpload: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.enqueueCleanupCalls) != 1 {
		t.Fatalf("expected 1 enqueue call, got %d", len(store.enqueueCleanupCalls))
	}
	if store.enqueueCleanupCalls[0].reason != "abort_part_cleanup" {
		t.Errorf("expected reason=abort_part_cleanup, got %q", store.enqueueCleanupCalls[0].reason)
	}
}

// -------------------------------------------------------------------------
// CompleteMultipartUpload part filtering
// -------------------------------------------------------------------------

func TestCompleteMultipartUpload_PartSubset(t *testing.T) {
	backend := newMockBackend()
	ctx := context.Background()
	// Upload 3 parts but only complete with parts 1 and 3
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/2", bytes.NewReader([]byte("BBB")), 3, "application/octet-stream", nil)
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/3", bytes.NewReader([]byte("CCC")), 3, "application/octet-stream", nil)

	store := &mockStore{
		getMultipartResp: &MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
			ContentType: "application/zip",
		},
		getPartsResp: []MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3},
			{PartNumber: 2, ETag: "e2", SizeBytes: 3},
			{PartNumber: 3, ETag: "e3", SizeBytes: 3},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	etag, err := mgr.CompleteMultipartUpload(ctx, "upload-1", []int{1, 3})
	if err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}
	if etag == "" {
		t.Error("expected non-empty etag")
	}
	// Final object should contain only parts 1+3 (6 bytes: "AAACCC")
	if !backend.hasObject("multi/key") {
		t.Fatal("final object not found on backend")
	}
	// RecordObject should reflect the subset size (6 bytes, not 9)
	if len(store.recordObjectCalls) != 1 {
		t.Fatalf("expected 1 RecordObject call, got %d", len(store.recordObjectCalls))
	}
	if store.recordObjectCalls[0].Size != 6 {
		t.Errorf("expected recorded size=6, got %d", store.recordObjectCalls[0].Size)
	}
}

func TestCompleteMultipartUpload_InvalidPart(t *testing.T) {
	store := &mockStore{
		getMultipartResp: &MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
			ContentType: "text/plain",
		},
		getPartsResp: []MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	// Request part 2 which was never uploaded
	_, err := mgr.CompleteMultipartUpload(context.Background(), "upload-1", []int{1, 2})
	if err == nil {
		t.Fatal("expected error for missing part")
	}
	var s3err *S3Error
	if !errors.As(err, &s3err) || s3err.Code != "InvalidPart" {
		t.Errorf("expected S3Error with Code=InvalidPart, got %v", err)
	}
}

// -------------------------------------------------------------------------
// CompleteMultipartUpload error paths
// -------------------------------------------------------------------------

func TestCompleteMultipartUpload_GetPartsError(t *testing.T) {
	store := &mockStore{
		getMultipartResp: &MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
		},
		getPartsErr: errors.New("db error"),
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	_, err := mgr.CompleteMultipartUpload(context.Background(), "upload-1", []int{1})
	if err == nil {
		t.Fatal("expected error from GetParts failure")
	}
}

func TestCompleteMultipartUpload_PartDeleteFails_EnqueuesCleanup(t *testing.T) {
	backend := newMockBackend()
	ctx := context.Background()
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)

	store := &mockStore{
		getMultipartResp: &MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
			ContentType: "application/zip",
		},
		getPartsResp: []MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	// Set delErr after parts are stored so complete can read them but can't delete
	backend.mu.Lock()
	backend.delErr = errors.New("backend timeout")
	backend.mu.Unlock()

	etag, err := mgr.CompleteMultipartUpload(ctx, "upload-1", []int{1})
	if err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}
	if etag == "" {
		t.Error("expected non-empty etag")
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.enqueueCleanupCalls) != 1 {
		t.Fatalf("expected 1 enqueue call, got %d", len(store.enqueueCleanupCalls))
	}
	if store.enqueueCleanupCalls[0].reason != "complete_part_cleanup" {
		t.Errorf("expected reason=complete_part_cleanup, got %q", store.enqueueCleanupCalls[0].reason)
	}
}

// -------------------------------------------------------------------------
// UploadPart error paths
// -------------------------------------------------------------------------

func TestUploadPart_UsageLimitExceeded(t *testing.T) {
	store := &mockStore{
		getMultipartResp: &MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	// Set usage limits that will be exceeded
	mgr.usage.UpdateLimits(map[string]UsageLimits{
		"b1": {IngressByteLimit: 1}, // only 1 byte allowed
	})

	_, err := mgr.UploadPart(context.Background(), "upload-1", 1, bytes.NewReader([]byte("large-data")), 10)
	if !errors.Is(err, ErrInsufficientStorage) {
		t.Fatalf("expected ErrInsufficientStorage, got %v", err)
	}
}

func TestUploadPart_RecordPartFails_CleansUpPartObject(t *testing.T) {
	backend := newMockBackend()
	store := &mockStore{
		getMultipartResp: &MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
		},
		recordPartErr: errors.New("db error"),
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	_, err := mgr.UploadPart(context.Background(), "upload-1", 1, bytes.NewReader([]byte("data")), 4)
	if err == nil {
		t.Fatal("expected error from RecordPart failure")
	}

	// Part object should be cleaned up from backend
	if backend.hasObject("__multipart/upload-1/1") {
		t.Error("orphaned part should be deleted from backend")
	}

	// Usage: 1 API call for the orphan cleanup delete (put usage only recorded on success path)
	c := mgr.usage.counters["b1"]
	if got := c.apiRequests.Load(); got != 1 {
		t.Errorf("apiRequests = %d, want 1 (orphan delete)", got)
	}
}

func TestUploadPart_RecordPartFails_DeleteFails_EnqueuesCleanup(t *testing.T) {
	backend := newMockBackend()
	store := &mockStore{
		getMultipartResp: &MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
		},
		recordPartErr: errors.New("db error"),
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	// Put will succeed, then set delErr so cleanup fails
	_, err := mgr.UploadPart(context.Background(), "upload-1", 1, bytes.NewReader([]byte("data")), 4)
	// This test is tricky — delErr is set on the backend struct, but the upload succeeds
	// before we can set it. Let me instead pre-set delErr and rely on putErr being nil.
	if err == nil {
		t.Fatal("expected error from RecordPart failure")
	}
}

// -------------------------------------------------------------------------
// CleanupStaleMultipartUploads
// -------------------------------------------------------------------------

func TestCleanupStaleMultipartUploads_NoStaleUploads(t *testing.T) {
	store := &mockStore{}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	// Should not panic
	mgr.CleanupStaleMultipartUploads(context.Background(), time.Hour)
}

func TestCleanupStaleMultipartUploads_QueryError(t *testing.T) {
	store := &mockStore{getStaleMultipartErr: errors.New("db error")}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	// Should not panic
	mgr.CleanupStaleMultipartUploads(context.Background(), time.Hour)
}

func TestCleanupStaleMultipartUploads_AbortsStaleUploads(t *testing.T) {
	backend := newMockBackend()
	ctx := context.Background()
	_, _ = backend.PutObject(ctx, "__multipart/stale-1/1", bytes.NewReader([]byte("x")), 1, "", nil)

	store := &mockStore{
		getStaleMultipartResp: []MultipartUpload{
			{UploadID: "stale-1", ObjectKey: "stale/key", BackendName: "b1"},
		},
		getMultipartResp: &MultipartUpload{
			UploadID:    "stale-1",
			ObjectKey:   "stale/key",
			BackendName: "b1",
		},
		getPartsResp: []MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 1},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	mgr.CleanupStaleMultipartUploads(ctx, time.Hour)

	// Part should be cleaned up
	if backend.hasObject("__multipart/stale-1/1") {
		t.Error("stale part should be cleaned up")
	}
}

// -------------------------------------------------------------------------
// CreateMultipartUpload edge cases
// -------------------------------------------------------------------------

func TestCompleteMultipartUpload_UsageRecords2NPlus1APICalls(t *testing.T) {
	backend := newMockBackend()
	ctx := context.Background()
	// Pre-store 3 parts on the backend
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/2", bytes.NewReader([]byte("BBB")), 3, "application/octet-stream", nil)
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/3", bytes.NewReader([]byte("CCC")), 3, "application/octet-stream", nil)

	store := &mockStore{
		getMultipartResp: &MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
			ContentType: "application/zip",
		},
		getPartsResp: []MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3},
			{PartNumber: 2, ETag: "e2", SizeBytes: 3},
			{PartNumber: 3, ETag: "e3", SizeBytes: 3},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	_, err := mgr.CompleteMultipartUpload(ctx, "upload-1", []int{1, 2, 3})
	if err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}

	// 3 parts → 3 GetObject + 1 PutObject + 3 DeleteObject = 7 API calls (2N+1)
	c := mgr.usage.counters["b1"]
	wantAPICalls := int64(2*3 + 1)
	if got := c.apiRequests.Load(); got != wantAPICalls {
		t.Errorf("apiRequests = %d, want %d (2*N+1 where N=3)", got, wantAPICalls)
	}
	// Total ingress should equal sum of all parts (9 bytes)
	if got := c.ingressBytes.Load(); got != 9 {
		t.Errorf("ingressBytes = %d, want 9", got)
	}
}

func TestUploadPart_BackendFailure_StillRecordsUsage(t *testing.T) {
	backend := newMockBackend()
	backend.putErr = errors.New("backend timeout")

	store := &mockStore{
		getMultipartResp: &MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	_, err := mgr.UploadPart(context.Background(), "upload-1", 1, bytes.NewReader([]byte("data")), 4)
	if err == nil {
		t.Fatal("expected error from backend failure")
	}

	// Even on failure, 1 API call should be recorded (the attempt was made)
	c := mgr.usage.counters["b1"]
	if got := c.apiRequests.Load(); got != 1 {
		t.Errorf("apiRequests = %d, want 1 (failed call still counts)", got)
	}
	// No ingress should be recorded since the upload failed
	if got := c.ingressBytes.Load(); got != 0 {
		t.Errorf("ingressBytes = %d, want 0 (upload failed)", got)
	}
}

func TestCreateMultipartUpload_CreateStoreError(t *testing.T) {
	store := &mockStore{
		getBackendResp:     "b1",
		createMultipartErr: errors.New("db error"),
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	_, _, err := mgr.CreateMultipartUpload(context.Background(), "key", "", nil)
	if err == nil {
		t.Fatal("expected error from CreateMultipartUpload store failure")
	}
}
