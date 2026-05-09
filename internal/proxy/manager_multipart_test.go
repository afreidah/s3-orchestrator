// -------------------------------------------------------------------------------
// Multipart Upload Tests
//
// Author: Alex Freidah
//
// Tests for BackendManager multipart upload operations: CreateMultipartUpload,
// UploadPart, CompleteMultipartUpload, and AbortMultipartUpload. Validates
// backend delegation, metadata recording, and error handling.
// -------------------------------------------------------------------------------

package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// CreateMultipartUpload
// -------------------------------------------------------------------------

// TestCreateMultipartUpload_Success verifies the create multipart upload success contract.
// Asserts that CreateMultipartUpload:.
func TestCreateMultipartUpload_Success(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	store := &mockStore{getBackendResp: "b1"}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	uploadID, backendName, err := mgr.MultipartManager.CreateMultipartUpload(context.Background(), "multi/key", "application/zip", nil)
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

// TestCreateMultipartUpload_DBUnavailable verifies the create multipart upload dbunavailable contract.
// Asserts that expected st.ErrServiceUnavailable, got.
func TestCreateMultipartUpload_DBUnavailable(t *testing.T) {
	t.Parallel()
	store := &mockStore{getBackendErr: core.ErrDBUnavailable}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	_, _, err := mgr.MultipartManager.CreateMultipartUpload(context.Background(), "key", "", nil)
	if !errors.Is(err, core.ErrServiceUnavailable) {
		t.Fatalf("expected st.ErrServiceUnavailable, got %v", err)
	}
}

// TestCreateMultipartUpload_NoSpace verifies the create multipart upload no space contract.
// Asserts that expected st.ErrInsufficientStorage, got.
func TestCreateMultipartUpload_NoSpace(t *testing.T) {
	t.Parallel()
	store := &mockStore{getBackendErr: core.ErrNoSpaceAvailable}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	_, _, err := mgr.MultipartManager.CreateMultipartUpload(context.Background(), "key", "", nil)
	if !errors.Is(err, core.ErrInsufficientStorage) {
		t.Fatalf("expected st.ErrInsufficientStorage, got %v", err)
	}
}

// -------------------------------------------------------------------------
// UploadPart
// -------------------------------------------------------------------------

// TestUploadPart_Success verifies the upload part success contract.
// Asserts that UploadPart:.
func TestUploadPart_Success(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	store := &mockStore{
		getMultipartResp: &core.MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
			ContentType: "application/zip",
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	etag, err := mgr.MultipartManager.UploadPart(context.Background(), "multi", "key", "upload-1", 1, bytes.NewReader([]byte("part-data")), 9)
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

// TestUploadPart_InvalidPartNumber verifies the upload part invalid part number contract.
// Asserts that UploadPart(partNumber=) should fail.
func TestUploadPart_InvalidPartNumber(t *testing.T) {
	t.Parallel()
	mgr := newTestManager(&mockStore{}, map[string]*mockBackend{"b1": newMockBackend()})

	for _, pn := range []int{0, -1, 10001, 1 << 20} {
		_, err := mgr.MultipartManager.UploadPart(context.Background(), "multi", "key", "upload-1", pn, bytes.NewReader([]byte("x")), 1)
		if err == nil {
			t.Errorf("UploadPart(partNumber=%d) should fail", pn)
			continue
		}
		var s3Err *core.S3Error
		if !errors.As(err, &s3Err) || s3Err.Code != "InvalidArgument" {
			t.Errorf("UploadPart(partNumber=%d) = %v, want st.S3Error InvalidArgument", pn, err)
		}
	}
}

// TestUploadPart_DBUnavailable verifies the upload part dbunavailable contract.
// Asserts that expected st.ErrServiceUnavailable, got.
func TestUploadPart_DBUnavailable(t *testing.T) {
	t.Parallel()
	store := &mockStore{getMultipartErr: core.ErrDBUnavailable}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	_, err := mgr.MultipartManager.UploadPart(context.Background(), "multi", "key", "upload-1", 1, bytes.NewReader([]byte("x")), 1)
	if !errors.Is(err, core.ErrServiceUnavailable) {
		t.Fatalf("expected st.ErrServiceUnavailable, got %v", err)
	}
}

// -------------------------------------------------------------------------
// CompleteMultipartUpload
// -------------------------------------------------------------------------

// TestCompleteMultipartUpload_Success verifies the complete multipart upload success contract.
// Asserts that CompleteMultipartUpload:.
func TestCompleteMultipartUpload_Success(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()

	// Pre-store parts on the backend
	ctx := context.Background()
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/2", bytes.NewReader([]byte("BBB")), 3, "application/octet-stream", nil)

	store := &mockStore{
		getMultipartResp: &core.MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
			ContentType: "application/zip",
		},
		getPartsResp: []core.MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3},
			{PartNumber: 2, ETag: "e2", SizeBytes: 3},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	etag, err := mgr.MultipartManager.CompleteMultipartUpload(ctx, "multi", "key", "upload-1", []int{1, 2})
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

// TestCompleteMultipartUpload_DBUnavailable verifies the complete multipart upload dbunavailable contract.
// Asserts that expected st.ErrServiceUnavailable, got.
func TestCompleteMultipartUpload_DBUnavailable(t *testing.T) {
	t.Parallel()
	store := &mockStore{getMultipartErr: core.ErrDBUnavailable}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	_, err := mgr.MultipartManager.CompleteMultipartUpload(context.Background(), "multi", "key", "upload-1", []int{1})
	if !errors.Is(err, core.ErrServiceUnavailable) {
		t.Fatalf("expected st.ErrServiceUnavailable, got %v", err)
	}
}

// -------------------------------------------------------------------------
// AbortMultipartUpload
// -------------------------------------------------------------------------

// TestAbortMultipartUpload_Success verifies the abort multipart upload success contract.
// Asserts that AbortMultipartUpload:.
func TestAbortMultipartUpload_Success(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	ctx := context.Background()
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)

	store := &mockStore{
		getMultipartResp: &core.MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
		},
		getPartsResp: []core.MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3, CreatedAt: time.Now()},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	err := mgr.MultipartManager.AbortMultipartUpload(ctx, "multi", "key", "upload-1")
	if err != nil {
		t.Fatalf("AbortMultipartUpload: %v", err)
	}
	// Part should be cleaned up
	if backend.hasObject("__multipart/upload-1/1") {
		t.Error("part temp key should be deleted")
	}

	// Usage: 1 part delete + 1 abort = 2 API calls
	if got := mgr.usage.Backend().Load("b1", counter.FieldAPIRequests); got != 2 {
		t.Errorf("apiRequests = %d, want 2 (1 part delete + 1 abort)", got)
	}
}

// TestAbortMultipartUpload_DBUnavailable verifies the abort multipart upload dbunavailable contract.
// Asserts that expected st.ErrServiceUnavailable, got.
func TestAbortMultipartUpload_DBUnavailable(t *testing.T) {
	t.Parallel()
	store := &mockStore{getMultipartErr: core.ErrDBUnavailable}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	err := mgr.MultipartManager.AbortMultipartUpload(context.Background(), "multi", "key", "upload-1")
	if !errors.Is(err, core.ErrServiceUnavailable) {
		t.Fatalf("expected st.ErrServiceUnavailable, got %v", err)
	}
}

// TestAbortMultipartUpload_GetPartsError verifies the abort multipart upload get parts error path by exercising errors.New, context.Background.
func TestAbortMultipartUpload_GetPartsError(t *testing.T) {
	t.Parallel()
	store := &mockStore{
		getMultipartResp: &core.MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
		},
		getPartsErr: errors.New("db error"),
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	err := mgr.MultipartManager.AbortMultipartUpload(context.Background(), "multi", "key", "upload-1")
	if err == nil {
		t.Fatal("expected error from GetParts failure")
	}
}

// TestAbortMultipartUpload_PartDeleteFails_EnqueuesCleanup verifies the abort multipart upload part delete fails enqueues cleanup contract.
// Asserts that AbortMultipartUpload:.
func TestAbortMultipartUpload_PartDeleteFails_EnqueuesCleanup(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	backend.delErr = errors.New("backend timeout")
	ctx := context.Background()
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)
	// Reset delErr after seeding (PutObject doesn't check it, but just in case)
	backend.mu.Lock()
	backend.delErr = errors.New("backend timeout")
	backend.mu.Unlock()

	store := &mockStore{
		getMultipartResp: &core.MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
		},
		getPartsResp: []core.MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	// Abort should still succeed (delete failure on backend just enqueues cleanup)
	// but DeleteMultipartUpload will also run
	err := mgr.MultipartManager.AbortMultipartUpload(ctx, "multi", "key", "upload-1")
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

// TestCompleteMultipartUpload_PartSubset verifies the complete multipart upload part subset contract.
// Asserts that CompleteMultipartUpload:.
func TestCompleteMultipartUpload_PartSubset(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	ctx := context.Background()
	// Upload 3 parts but only complete with parts 1 and 3
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/2", bytes.NewReader([]byte("BBB")), 3, "application/octet-stream", nil)
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/3", bytes.NewReader([]byte("CCC")), 3, "application/octet-stream", nil)

	store := &mockStore{
		getMultipartResp: &core.MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
			ContentType: "application/zip",
		},
		getPartsResp: []core.MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3},
			{PartNumber: 2, ETag: "e2", SizeBytes: 3},
			{PartNumber: 3, ETag: "e3", SizeBytes: 3},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	etag, err := mgr.MultipartManager.CompleteMultipartUpload(ctx, "multi", "key", "upload-1", []int{1, 3})
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

// TestCompleteMultipartUpload_InvalidPart verifies the complete multipart upload invalid part contract.
// Asserts that expected st.S3Error with Code=InvalidPart, got.
func TestCompleteMultipartUpload_InvalidPart(t *testing.T) {
	t.Parallel()
	store := &mockStore{
		getMultipartResp: &core.MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
			ContentType: "text/plain",
		},
		getPartsResp: []core.MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	// Request part 2 which was never uploaded
	_, err := mgr.MultipartManager.CompleteMultipartUpload(context.Background(), "multi", "key", "upload-1", []int{1, 2})
	if err == nil {
		t.Fatal("expected error for missing part")
	}
	var s3err *core.S3Error
	if !errors.As(err, &s3err) || s3err.Code != "InvalidPart" {
		t.Errorf("expected st.S3Error with Code=InvalidPart, got %v", err)
	}
}

// -------------------------------------------------------------------------
// CompleteMultipartUpload error paths
// -------------------------------------------------------------------------

// TestCompleteMultipartUpload_LockContended verifies that a Complete
// call returns 409 OperationAborted when the per-uploadID advisory
// lock is already held by another in-flight call. No backend or DB
// activity beyond the lock check should occur.
func TestCompleteMultipartUpload_LockContended(t *testing.T) {
	t.Parallel()
	store := &mockStore{
		advisoryLockBlocked: true,
		// Populate so we can confirm the locked branch is the only
		// reason the call fails: if the lock weren't checked first,
		// the test would pass through and try to assemble.
		getMultipartResp: &core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1"},
		getPartsResp:     []core.MultipartPart{{PartNumber: 1, ETag: "e1", SizeBytes: 3}},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	_, err := mgr.MultipartManager.CompleteMultipartUpload(context.Background(), "multi", "key", "upload-1", []int{1})
	if err == nil {
		t.Fatal("expected OperationAborted error from contended lock")
	}
	var s3err *core.S3Error
	if !errors.As(err, &s3err) {
		t.Fatalf("expected *core.S3Error, got %T: %v", err, err)
	}
	if s3err.StatusCode != 409 || s3err.Code != "OperationAborted" {
		t.Errorf("expected 409 OperationAborted, got %d %s", s3err.StatusCode, s3err.Code)
	}
	// No RecordObject call expected: the locked branch never reached assembly.
	if len(store.recordObjectCalls) != 0 {
		t.Errorf("expected 0 RecordObject calls, got %d", len(store.recordObjectCalls))
	}
}

// TestCompleteMultipartUpload_AssemblyFails_CleansUpParts verifies
// that when the assembled PutObject fails, the deferred cleanup still
// fires: each part object is deleted from the backend and the
// multipart_uploads metadata row is removed. This prevents the orphan
// the issue #650 surfaced where the failure path was leaking parts
// until the periodic stale-multipart sweeper could catch them.
func TestCompleteMultipartUpload_AssemblyFails_CleansUpParts(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	ctx := context.Background()
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/2", bytes.NewReader([]byte("BBB")), 3, "application/octet-stream", nil)

	store := &mockStore{
		getMultipartResp: &core.MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
			ContentType: "application/zip",
		},
		getPartsResp: []core.MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3},
			{PartNumber: 2, ETag: "e2", SizeBytes: 3},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	// Inject the assembly failure. Parts are already pre-staged so they
	// stream out via GetObject; the only PutObject in the test path is
	// the assembly write that we want to fail.
	backend.putErr = errors.New("backend write failed")

	_, err := mgr.MultipartManager.CompleteMultipartUpload(ctx, "multi", "key", "upload-1", []int{1, 2})
	if err == nil {
		t.Fatal("expected CompleteMultipartUpload to fail")
	}

	if backend.hasObject("__multipart/upload-1/1") {
		t.Error("part 1 should have been deleted by the deferred cleanup")
	}
	if backend.hasObject("__multipart/upload-1/2") {
		t.Error("part 2 should have been deleted by the deferred cleanup")
	}
	if !store.deleteMultipartCalled {
		t.Error("expected DeleteMultipartUpload to be called by the deferred cleanup")
	}
	if backend.hasObject("multi/key") {
		t.Error("assembled key should not exist when the assembly PUT failed")
	}
}

// TestCompleteMultipartUpload_GetPartsError verifies the complete multipart upload get parts error path by exercising errors.New, context.Background.
func TestCompleteMultipartUpload_GetPartsError(t *testing.T) {
	t.Parallel()
	store := &mockStore{
		getMultipartResp: &core.MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
		},
		getPartsErr: errors.New("db error"),
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	_, err := mgr.MultipartManager.CompleteMultipartUpload(context.Background(), "multi", "key", "upload-1", []int{1})
	if err == nil {
		t.Fatal("expected error from GetParts failure")
	}
}

// TestCompleteMultipartUpload_PartDeleteFails_EnqueuesCleanup verifies the complete multipart upload part delete fails enqueues cleanup contract.
// Asserts that CompleteMultipartUpload:.
func TestCompleteMultipartUpload_PartDeleteFails_EnqueuesCleanup(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	ctx := context.Background()
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)

	store := &mockStore{
		getMultipartResp: &core.MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
			ContentType: "application/zip",
		},
		getPartsResp: []core.MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	// Set delErr after parts are stored so complete can read them but can't delete
	backend.mu.Lock()
	backend.delErr = errors.New("backend timeout")
	backend.mu.Unlock()

	etag, err := mgr.MultipartManager.CompleteMultipartUpload(ctx, "multi", "key", "upload-1", []int{1})
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

// TestCompleteMultipartUpload_FinalPutFails verifies the complete multipart upload final put fails path by exercising context.Background, backend.PutObject, bytes.NewReader.
func TestCompleteMultipartUpload_FinalPutFails(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	ctx := context.Background()
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)
	backend.putErr = errors.New("write failed")

	store := &mockStore{
		getMultipartResp: &core.MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
			ContentType: "application/zip",
		},
		getPartsResp: []core.MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	_, err := mgr.MultipartManager.CompleteMultipartUpload(ctx, "multi", "key", "upload-1", []int{1})
	if err == nil {
		t.Fatal("expected error when final PutObject fails")
	}
}

// TestCompleteMultipartUpload_PartReadFails verifies the complete multipart upload part read fails path by exercising context.Background, backend.PutObject, bytes.NewReader.
func TestCompleteMultipartUpload_PartReadFails(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	ctx := context.Background()
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)
	backend.getReadErr = errors.New("disk I/O error")

	store := &mockStore{
		getMultipartResp: &core.MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
			ContentType: "application/zip",
		},
		getPartsResp: []core.MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	_, err := mgr.MultipartManager.CompleteMultipartUpload(ctx, "multi", "key", "upload-1", []int{1})
	if err == nil {
		t.Fatal("expected error when part body read fails")
	}
}

// -------------------------------------------------------------------------
// UploadPart error paths
// -------------------------------------------------------------------------

// TestUploadPart_UsageLimitExceeded verifies the upload part usage limit exceeded contract.
// Asserts that expected st.ErrInsufficientStorage, got.
func TestUploadPart_UsageLimitExceeded(t *testing.T) {
	t.Parallel()
	store := &mockStore{
		getMultipartResp: &core.MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	// Set usage limits that will be exceeded
	mgr.usage.UpdateLimits(map[string]core.UsageLimits{
		"b1": {IngressByteLimit: 1}, // only 1 byte allowed
	})

	_, err := mgr.MultipartManager.UploadPart(context.Background(), "multi", "key", "upload-1", 1, bytes.NewReader([]byte("large-data")), 10)
	if !errors.Is(err, core.ErrInsufficientStorage) {
		t.Fatalf("expected st.ErrInsufficientStorage, got %v", err)
	}
}

// TestUploadPart_RecordPartFails_CleansUpPartObject verifies the upload part record part fails cleans up part object contract.
// Asserts that apiRequests = , want 2 (PUT + orphan DELETE).
func TestUploadPart_RecordPartFails_CleansUpPartObject(t *testing.T) {
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

	_, err := mgr.MultipartManager.UploadPart(context.Background(), "multi", "key", "upload-1", 1, bytes.NewReader([]byte("data")), 4)
	if err == nil {
		t.Fatal("expected error from RecordPart failure")
	}

	// Part object should be cleaned up from backend
	if backend.hasObject("__multipart/upload-1/1") {
		t.Error("orphaned part should be deleted from backend")
	}

	// Usage: 2 API calls  -  the part PUT that succeeded against the backend
	// and the cleanup DELETE that ran after RecordPart failed.
	if got := mgr.usage.Backend().Load("b1", counter.FieldAPIRequests); got != 2 {
		t.Errorf("apiRequests = %d, want 2 (PUT + orphan DELETE)", got)
	}
}

// TestUploadPart_RecordPartFails_DeleteFails_EnqueuesCleanup verifies the upload part record part fails delete fails enqueues cleanup contract.
// Asserts that expected 1 enqueue call, got.
func TestUploadPart_RecordPartFails_DeleteFails_EnqueuesCleanup(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	// Use putThenFailDelete: put succeeds, but subsequent deletes fail.
	// The mock backend's PutObject always works; set delErr before the call
	// so that when RecordPart fails and the cleanup delete runs, it also fails.
	backend.delErr = errors.New("backend timeout")

	store := &mockStore{
		getMultipartResp: &core.MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
		},
		recordPartErr: errors.New("db error"),
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	_, err := mgr.MultipartManager.UploadPart(context.Background(), "multi", "key", "upload-1", 1, bytes.NewReader([]byte("data")), 4)
	if err == nil {
		t.Fatal("expected error from RecordPart failure")
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	// The cleanup delete failed, so the orphan should be enqueued
	if len(store.enqueueCleanupCalls) != 1 {
		t.Fatalf("expected 1 enqueue call, got %d", len(store.enqueueCleanupCalls))
	}
	if store.enqueueCleanupCalls[0].reason != "orphan_part_record_failed" {
		t.Errorf("reason = %q, want orphan_part_record_failed", store.enqueueCleanupCalls[0].reason)
	}
	// Orphan bytes should be incremented
	if len(store.incrementOrphanBytesCalls) != 1 {
		t.Fatalf("expected 1 IncrementOrphanBytes call, got %d", len(store.incrementOrphanBytesCalls))
	}
	if store.incrementOrphanBytesCalls[0].sizeBytes != 4 {
		t.Errorf("orphan bytes = %d, want 4", store.incrementOrphanBytesCalls[0].sizeBytes)
	}
}

// -------------------------------------------------------------------------
// CleanupStaleMultipartUploads
// -------------------------------------------------------------------------

// TestCleanupStaleMultipartUploads_NoStaleUploads verifies the cleanup stale multipart uploads no stale uploads path by exercising context.Background.
func TestCleanupStaleMultipartUploads_NoStaleUploads(t *testing.T) {
	t.Parallel()
	store := &mockStore{}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	// Should not panic
	mgr.MultipartManager.CleanupStaleMultipartUploads(context.Background(), time.Hour)
}

// TestCleanupStaleMultipartUploads_QueryError verifies the cleanup stale multipart uploads query error path by exercising errors.New, context.Background.
func TestCleanupStaleMultipartUploads_QueryError(t *testing.T) {
	t.Parallel()
	store := &mockStore{getStaleMultipartErr: errors.New("db error")}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	// Should not panic
	mgr.MultipartManager.CleanupStaleMultipartUploads(context.Background(), time.Hour)
}

// TestCleanupStaleMultipartUploads_AbortsStaleUploads verifies the cleanup stale multipart uploads aborts stale uploads path by exercising context.Background, backend.PutObject, bytes.NewReader.
func TestCleanupStaleMultipartUploads_AbortsStaleUploads(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	ctx := context.Background()
	_, _ = backend.PutObject(ctx, "__multipart/stale-1/1", bytes.NewReader([]byte("x")), 1, "", nil)

	store := &mockStore{
		getStaleMultipartResp: []core.MultipartUpload{
			{UploadID: "stale-1", ObjectKey: "stale/key", BackendName: "b1"},
		},
		getMultipartResp: &core.MultipartUpload{
			UploadID:    "stale-1",
			ObjectKey:   "stale/key",
			BackendName: "b1",
		},
		getPartsResp: []core.MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 1},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	mgr.MultipartManager.CleanupStaleMultipartUploads(ctx, time.Hour)

	// Part should be cleaned up
	if backend.hasObject("__multipart/stale-1/1") {
		t.Error("stale part should be cleaned up")
	}
}

// -------------------------------------------------------------------------
// CreateMultipartUpload edge cases
// -------------------------------------------------------------------------

// TestCompleteMultipartUpload_UsageRecords2NPlus1APICalls verifies the complete multipart upload usage records2 nplus1 apicalls contract.
// Asserts that CompleteMultipartUpload:.
func TestCompleteMultipartUpload_UsageRecords2NPlus1APICalls(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	ctx := context.Background()
	// Pre-store 3 parts on the backend
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/2", bytes.NewReader([]byte("BBB")), 3, "application/octet-stream", nil)
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/3", bytes.NewReader([]byte("CCC")), 3, "application/octet-stream", nil)

	store := &mockStore{
		getMultipartResp: &core.MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
			ContentType: "application/zip",
		},
		getPartsResp: []core.MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3},
			{PartNumber: 2, ETag: "e2", SizeBytes: 3},
			{PartNumber: 3, ETag: "e3", SizeBytes: 3},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	_, err := mgr.MultipartManager.CompleteMultipartUpload(ctx, "multi", "key", "upload-1", []int{1, 2, 3})
	if err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}

	// 3 parts -- 3 GetObject + 1 PutObject + 3 DeleteObject = 7 API calls (2N+1)
	wantAPICalls := int64(2*3 + 1)
	if got := mgr.usage.Backend().Load("b1", counter.FieldAPIRequests); got != wantAPICalls {
		t.Errorf("apiRequests = %d, want %d (2*N+1 where N=3)", got, wantAPICalls)
	}
	// Total ingress should equal sum of all parts (9 bytes)
	if got := mgr.usage.Backend().Load("b1", counter.FieldIngressBytes); got != 9 {
		t.Errorf("ingressBytes = %d, want 9", got)
	}
}

// TestUploadPart_BackendFailure_StillRecordsUsage verifies the upload part backend failure still records usage contract.
// Asserts that apiRequests = , want 1 (failed call still counts).
func TestUploadPart_BackendFailure_StillRecordsUsage(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	backend.putErr = errors.New("backend timeout")

	store := &mockStore{
		getMultipartResp: &core.MultipartUpload{
			UploadID:    "upload-1",
			ObjectKey:   "multi/key",
			BackendName: "b1",
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	_, err := mgr.MultipartManager.UploadPart(context.Background(), "multi", "key", "upload-1", 1, bytes.NewReader([]byte("data")), 4)
	if err == nil {
		t.Fatal("expected error from backend failure")
	}

	// Even on failure, 1 API call should be recorded (the attempt was made)
	if got := mgr.usage.Backend().Load("b1", counter.FieldAPIRequests); got != 1 {
		t.Errorf("apiRequests = %d, want 1 (failed call still counts)", got)
	}
	// No ingress should be recorded since the upload failed
	if got := mgr.usage.Backend().Load("b1", counter.FieldIngressBytes); got != 0 {
		t.Errorf("ingressBytes = %d, want 0 (upload failed)", got)
	}
}

// TestCreateMultipartUpload_CreateStoreError verifies the create multipart upload create store error path by exercising errors.New, context.Background.
func TestCreateMultipartUpload_CreateStoreError(t *testing.T) {
	t.Parallel()
	store := &mockStore{
		getBackendResp:     "b1",
		createMultipartErr: errors.New("db error"),
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	_, _, err := mgr.MultipartManager.CreateMultipartUpload(context.Background(), "key", "", nil)
	if err == nil {
		t.Fatal("expected error from CreateMultipartUpload store failure")
	}
}

// -------------------------------------------------------------------------
// CleanupStaleMultipartUploads  -  abort failure path
// -------------------------------------------------------------------------

// TestCleanupStaleMultipartUploads_AbortFails verifies the cleanup stale multipart uploads abort fails path by exercising errors.New, context.Background.
func TestCleanupStaleMultipartUploads_AbortFails(t *testing.T) {
	t.Parallel()
	store := &mockStore{
		getStaleMultipartResp: []core.MultipartUpload{
			{UploadID: "stale-1", ObjectKey: "stale/key", BackendName: "b1"},
		},
		// GetMultipartUpload will fail, causing AbortMultipartUpload to fail
		getMultipartErr: errors.New("db error"),
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	// Should not panic  -  logs the error and continues
	mgr.MultipartManager.CleanupStaleMultipartUploads(context.Background(), time.Hour)
}

// -------------------------------------------------------------------------
// AbortMultipartUploadsOnBackend
// -------------------------------------------------------------------------

// TestAbortMultipartUploadsOnBackend_ListError verifies the abort multipart uploads on backend list error path by exercising errors.New, context.Background.
func TestAbortMultipartUploadsOnBackend_ListError(t *testing.T) {
	t.Parallel()
	store := &mockStore{
		getStaleMultipartErr: errors.New("db error"),
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	// Should not panic  -  logs error and returns early
	mgr.MultipartManager.AbortMultipartUploadsOnBackend(context.Background(), "b1")
}

// TestAbortMultipartUploadsOnBackend_AbortsMatchingBackend verifies the abort multipart uploads on backend aborts matching backend path by exercising context.Background, backend.PutObject, bytes.NewReader.
func TestAbortMultipartUploadsOnBackend_AbortsMatchingBackend(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	ctx := context.Background()
	_, _ = backend.PutObject(ctx, "__multipart/up-1/1", bytes.NewReader([]byte("x")), 1, "", nil)

	store := &mockStore{
		getStaleMultipartResp: []core.MultipartUpload{
			{UploadID: "up-1", ObjectKey: "key1", BackendName: "b1"},
			{UploadID: "up-2", ObjectKey: "key2", BackendName: "b2"}, // different backend  -  skipped
		},
		getMultipartResp: &core.MultipartUpload{
			UploadID:    "up-1",
			ObjectKey:   "key1",
			BackendName: "b1",
		},
		getPartsResp: []core.MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 1},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	mgr.MultipartManager.AbortMultipartUploadsOnBackend(ctx, "b1")

	// Part should be cleaned up
	if backend.hasObject("__multipart/up-1/1") {
		t.Error("stale part should be cleaned up")
	}
}

// TestAbortMultipartUploadsOnBackend_AbortFails verifies the abort multipart uploads on backend abort fails path by exercising errors.New, context.Background.
func TestAbortMultipartUploadsOnBackend_AbortFails(t *testing.T) {
	t.Parallel()
	store := &mockStore{
		getStaleMultipartResp: []core.MultipartUpload{
			{UploadID: "up-1", ObjectKey: "key1", BackendName: "b1"},
		},
		getMultipartErr: errors.New("db error"), // causes AbortMultipartUpload to fail
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	// Should not panic  -  logs error and continues
	mgr.MultipartManager.AbortMultipartUploadsOnBackend(context.Background(), "b1")
}

// newEncryptedTestManager wires a manager with a real Encryptor so the
// shared-DEK code paths (unwrapUploadDEK, encryption-aware UploadPart,
// buildAssembledUpload) can be exercised in unit tests.
func newEncryptedTestManager(t *testing.T, store *mockStore, backends map[string]*mockBackend) *BackendManager {
	t.Helper()
	provider, err := encryption.NewConfigKeyProvider("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "test-0")
	if err != nil {
		t.Fatalf("NewConfigKeyProvider: %v", err)
	}
	enc, err := encryption.NewEncryptor(provider, 64*1024)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	obs := make(map[string]s3be.ObjectBackend, len(backends))
	var order []string
	for name, b := range backends {
		obs[name] = b
		order = append(order, name)
	}
	return wireWorkersForTest(NewBackendManager(&BackendManagerConfig{
		Backends:        obs,
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           order,
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
		Encryptor:       enc,
	}))
}

// failingKeyProvider mirrors the failing provider in
// internal/encryption/encryption_test.go but lives in this package
// so the proxy tests can wire an Encryptor whose WrapDEK call
// fails. Drives the wrap-error branches in CreateMultipartUpload.
type failingKeyProvider struct{}

func (failingKeyProvider) WrapDEK(_ context.Context, _ []byte) ([]byte, string, error) {
	return nil, "", errors.New("simulated wrap failure")
}
func (failingKeyProvider) UnwrapDEK(_ context.Context, _ []byte, _ string) ([]byte, error) {
	return nil, errors.New("simulated unwrap failure")
}
func (failingKeyProvider) KeyID() string { return "fail-0" }

// newFailingEncryptionTestManager wires a manager whose Encryptor
// has a KeyProvider that always fails WrapDEK / UnwrapDEK. Lets the
// CreateMultipartUpload wrap-error branch run.
func newFailingEncryptionTestManager(t *testing.T, store *mockStore, backends map[string]*mockBackend) *BackendManager {
	t.Helper()
	enc, err := encryption.NewEncryptor(failingKeyProvider{}, 64*1024)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	obs := make(map[string]s3be.ObjectBackend, len(backends))
	var order []string
	for name, b := range backends {
		obs[name] = b
		order = append(order, name)
	}
	return wireWorkersForTest(NewBackendManager(&BackendManagerConfig{
		Backends:        obs,
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           order,
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
		Encryptor:       enc,
	}))
}

// TestCreateMultipartUpload_WrapDEKError covers the
// CreateMultipartUpload branch where GenerateAndWrapDEK fails. The
// caller must surface the wrap error, not silently fall through.
func TestCreateMultipartUpload_WrapDEKError(t *testing.T) {
	t.Parallel()
	store := &mockStore{getBackendResp: "b1"}
	mgr := newFailingEncryptionTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})
	_, _, err := mgr.MultipartManager.CreateMultipartUpload(context.Background(), "k", "", nil)
	if err == nil {
		t.Fatal("expected error from wrap failure, got nil")
	}
}

// TestCreateMultipartUpload_EncryptionWrapsSharedDEK covers the
// branch in CreateMultipartUpload that wraps a shared upload DEK
// once and persists it on the upload row. Earlier per-part wrapping
// was eliminated by #650 item 3.
func TestCreateMultipartUpload_EncryptionWrapsSharedDEK(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	store := &mockStore{getBackendResp: "b1"}
	mgr := newEncryptedTestManager(t, store, map[string]*mockBackend{"b1": backend})

	_, _, err := mgr.MultipartManager.CreateMultipartUpload(context.Background(), "k", "application/zip", nil)
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	if len(store.createMultipartCalls) != 1 {
		t.Fatalf("expected 1 CreateMultipartUpload call, got %d", len(store.createMultipartCalls))
	}
	got := store.createMultipartCalls[0]
	if len(got.EncryptionKey) == 0 {
		t.Error("upload row missing wrapped EncryptionKey")
	}
	if got.KeyID == "" {
		t.Error("upload row missing KeyID")
	}
}

// TestUploadPart_ReusesSharedDEK exercises the encryption branch in
// UploadPart: the upload row already carries a wrapped DEK, the part
// is encrypted under it (no fresh WrapDEK call), and the part row
// receives encryption metadata.
func TestUploadPart_ReusesSharedDEK(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	store := &mockStore{getBackendResp: "b1"}
	mgr := newEncryptedTestManager(t, store, map[string]*mockBackend{"b1": backend})

	_, _, err := mgr.MultipartManager.CreateMultipartUpload(context.Background(), "multi/k", "", nil)
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	created := store.createMultipartCalls[0]
	store.getMultipartResp = &core.MultipartUpload{
		UploadID:      created.UploadID,
		ObjectKey:     created.ObjectKey,
		BackendName:   created.BackendName,
		Encrypted:     true,
		EncryptionKey: created.EncryptionKey,
		KeyID:         created.KeyID,
	}

	if _, err := mgr.MultipartManager.UploadPart(context.Background(), "multi", "k", created.UploadID, 1, bytes.NewReader([]byte("part-1-bytes")), 12); err != nil {
		t.Fatalf("UploadPart: %v", err)
	}
}

// TestCompleteMultipartUpload_Encrypted_RoundTrips covers the
// shared-DEK assembly path (buildAssembledUpload + decrypt-each-part
// streaming) end-to-end against an in-memory backend. After
// CompleteMultipartUpload the assembled object's plaintext should
// reconstitute the parts.
func TestCompleteMultipartUpload_Encrypted_RoundTrips(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	store := &mockStore{getBackendResp: "b1"}
	mgr := newEncryptedTestManager(t, store, map[string]*mockBackend{"b1": backend})
	ctx := context.Background()

	uploadID, _, err := mgr.MultipartManager.CreateMultipartUpload(ctx, "multi/k", "", nil)
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	created := store.createMultipartCalls[0]
	mu := &core.MultipartUpload{
		UploadID:      uploadID,
		ObjectKey:     created.ObjectKey,
		BackendName:   created.BackendName,
		Encrypted:     true,
		EncryptionKey: created.EncryptionKey,
		KeyID:         created.KeyID,
	}
	store.getMultipartResp = mu

	parts := [][]byte{[]byte("hello-"), []byte("world!")}
	for i, p := range parts {
		if _, err := mgr.MultipartManager.UploadPart(ctx, "multi", "k", uploadID, i+1, bytes.NewReader(p), int64(len(p))); err != nil {
			t.Fatalf("UploadPart %d: %v", i+1, err)
		}
	}
	store.getPartsResp = []core.MultipartPart{
		{PartNumber: 1, ETag: "e1", SizeBytes: int64(backendObjectSize(backend, "__multipart/"+uploadID+"/1")), Encrypted: true, EncryptionKey: store.recordPartCalls[0].Enc.EncryptionKey, KeyID: store.recordPartCalls[0].Enc.KeyID, PlaintextSize: 6},
		{PartNumber: 2, ETag: "e2", SizeBytes: int64(backendObjectSize(backend, "__multipart/"+uploadID+"/2")), Encrypted: true, EncryptionKey: store.recordPartCalls[1].Enc.EncryptionKey, KeyID: store.recordPartCalls[1].Enc.KeyID, PlaintextSize: 6},
	}
	if _, err := mgr.MultipartManager.CompleteMultipartUpload(ctx, "multi", "k", uploadID, []int{1, 2}); err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}
}

// TestUnwrapUploadDEK_NoEncryptionMetadata covers the
// unwrapUploadDEK branch where the upload row was not flagged as
// encrypted (assembly logic should never call this path; the
// guardrail exists in case a caller drifts).
func TestUnwrapUploadDEK_NoEncryptionMetadata(t *testing.T) {
	t.Parallel()
	mgr := newEncryptedTestManager(t, &mockStore{}, map[string]*mockBackend{"b1": newMockBackend()})
	mu := &core.MultipartUpload{UploadID: "u1", Encrypted: false}
	_, _, _, err := mgr.MultipartManager.unwrapUploadDEK(context.Background(), mu)
	if err == nil {
		t.Fatal("expected error for unencrypted upload, got nil")
	}
}

// TestUnwrapUploadDEK_UnpackError covers the branch where the upload
// row's EncryptionKey is too short for UnpackKeyData to split.
func TestUnwrapUploadDEK_UnpackError(t *testing.T) {
	t.Parallel()
	mgr := newEncryptedTestManager(t, &mockStore{}, map[string]*mockBackend{"b1": newMockBackend()})
	mu := &core.MultipartUpload{
		UploadID:      "u1",
		Encrypted:     true,
		EncryptionKey: []byte{0x01, 0x02},
		KeyID:         "kid",
	}
	_, _, _, err := mgr.MultipartManager.unwrapUploadDEK(context.Background(), mu)
	if err == nil {
		t.Fatal("expected error from UnpackKeyData, got nil")
	}
}

// TestUnwrapUploadDEK_UnwrapFails covers the branch where
// UnpackKeyData succeeds but the KeyProvider rejects the wrapped DEK
// during UnwrapDEK.
func TestUnwrapUploadDEK_UnwrapFails(t *testing.T) {
	t.Parallel()
	mgr := newEncryptedTestManager(t, &mockStore{}, map[string]*mockBackend{"b1": newMockBackend()})
	bogus := encryption.PackKeyData(make([]byte, encryption.NonceSize), []byte("not-a-real-wrapped-dek"))
	mu := &core.MultipartUpload{
		UploadID:      "u1",
		Encrypted:     true,
		EncryptionKey: bogus,
		KeyID:         "test-0",
	}
	_, _, _, err := mgr.MultipartManager.unwrapUploadDEK(context.Background(), mu)
	if err == nil {
		t.Fatal("expected unwrap error, got nil")
	}
}

// TestUploadPart_UnwrapDEKError covers the UploadPart branch where
// the upload row's wrapped DEK cannot be unwrapped (corrupted bytes
// or revoked key). Caller surfaces the error rather than uploading
// unencrypted bytes.
func TestUploadPart_UnwrapDEKError(t *testing.T) {
	t.Parallel()
	store := &mockStore{getMultipartResp: &core.MultipartUpload{
		UploadID:      "u1",
		ObjectKey:     "multi/k",
		BackendName:   "b1",
		Encrypted:     true,
		EncryptionKey: encryption.PackKeyData(make([]byte, encryption.NonceSize), []byte("not-a-real-wrapped-dek")),
		KeyID:         "test-0",
	}}
	mgr := newEncryptedTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})
	_, err := mgr.MultipartManager.UploadPart(context.Background(), "multi", "k", "u1", 1, bytes.NewReader([]byte("data")), 4)
	if err == nil {
		t.Fatal("expected unwrap error from UploadPart, got nil")
	}
}

// TestCompleteMultipartUpload_UnwrapDEKError covers the
// buildAssembledUpload branch where the upload row's wrapped DEK
// cannot be unwrapped during final assembly.
func TestCompleteMultipartUpload_UnwrapDEKError(t *testing.T) {
	t.Parallel()
	store := &mockStore{
		getMultipartResp: &core.MultipartUpload{
			UploadID:      "u1",
			ObjectKey:     "multi/k",
			BackendName:   "b1",
			Encrypted:     true,
			EncryptionKey: encryption.PackKeyData(make([]byte, encryption.NonceSize), []byte("not-a-real-wrapped-dek")),
			KeyID:         "test-0",
		},
		getPartsResp: []core.MultipartPart{{PartNumber: 1, ETag: "e", SizeBytes: 1, Encrypted: false}},
	}
	mgr := newEncryptedTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})
	_, err := mgr.MultipartManager.CompleteMultipartUpload(context.Background(), "multi", "k", "u1", []int{1})
	if err == nil {
		t.Fatal("expected unwrap error from Complete, got nil")
	}
}

// TestListMultipartUploads_PassThrough covers the manager
// pass-through wrapper for the metadata-store query.
func TestListMultipartUploads_PassThrough(t *testing.T) {
	t.Parallel()
	want := []core.MultipartUpload{{UploadID: "u1"}, {UploadID: "u2"}}
	store := &mockStore{listMultipartResp: want}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})
	got, err := mgr.MultipartManager.ListMultipartUploads(context.Background(), "p", 10)
	if err != nil {
		t.Fatalf("ListMultipartUploads: %v", err)
	}
	if len(got) != len(want) {
		t.Errorf("len = %d, want %d", len(got), len(want))
	}
}

// TestGetParts_PassThrough covers the manager pass-through wrapper
// for the metadata-store query.
func TestGetParts_PassThrough(t *testing.T) {
	t.Parallel()
	want := []core.MultipartPart{{PartNumber: 1, ETag: "e1"}}
	store := &mockStore{
		getMultipartResp: &core.MultipartUpload{UploadID: "u1", ObjectKey: "multi/key", BackendName: "b1"},
		getPartsResp:     want,
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})
	got, err := mgr.MultipartManager.GetParts(context.Background(), "multi", "key", "u1")
	if err != nil {
		t.Fatalf("GetParts: %v", err)
	}
	if len(got) != 1 || got[0].PartNumber != 1 {
		t.Errorf("got = %+v, want %+v", got, want)
	}
}

// backendObjectSize returns the size of an object stored in the
// in-memory mockBackend so the test can populate part rows with
// realistic ciphertext sizes after upload.
func backendObjectSize(b *mockBackend, key string) int {
	r, err := b.GetObject(context.Background(), key, "")
	if err != nil {
		return 0
	}
	defer r.Body.Close() //nolint:errcheck // best-effort close
	data, _ := io.ReadAll(r.Body)
	return len(data)
}

// TestCompleteMultipartUpload_PartGetPanics verifies that a panic inside the
// multipart assembly goroutine is recovered and surfaced as an error instead
// of deadlocking the request on the io.Pipe.
func TestCompleteMultipartUpload_PartGetPanics(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	backend.getPanic = true // causes GetObject to panic during part assembly

	store := &mockStore{
		getMultipartResp: &core.MultipartUpload{
			UploadID:    "upload-panic",
			ObjectKey:   "multi/panic",
			BackendName: "b1",
		},
		getPartsResp: []core.MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	_, err := mgr.MultipartManager.CompleteMultipartUpload(context.Background(), "multi", "panic", "upload-panic", []int{1})
	if err == nil {
		t.Fatal("expected error from panicking part reader, got nil")
	}
}
