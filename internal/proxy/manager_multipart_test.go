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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// multipartCalls captures store interactions a multipart test wants to
// assert on.
type multipartCalls struct {
	mu                  sync.Mutex
	create              []core.CreateMultipartUploadParams
	deleteMultipartHit  bool
	recordPart          []multipartPartCall
	recordObject        []multipartObjectCall
	enqueue             []core.CleanupItem
	incrementOrphan     []orphanBytesEntry
}

type multipartPartCall struct {
	uploadID  string
	partNumber int
	etag      string
	sizeBytes int64
	enc       *core.EncryptionMeta
}

type multipartObjectCall struct {
	Key, Backend string
	Size         int64
	Enc          *core.EncryptionMeta // pinned so tests can assert ContentHash etc.
}

func stubCreateMultipart(c *multipartCalls, err error) func(context.Context, *core.CreateMultipartUploadParams) error {
	return func(_ context.Context, params *core.CreateMultipartUploadParams) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.create = append(c.create, *params)
		return err
	}
}

func stubDeleteMultipart(c *multipartCalls) func(context.Context, string) error {
	return func(_ context.Context, _ string) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.deleteMultipartHit = true
		return nil
	}
}

func stubRecordPart(c *multipartCalls, err error) func(context.Context, string, int, string, int64, *core.EncryptionMeta) error {
	return func(_ context.Context, uploadID string, partNumber int, etag string, size int64, enc *core.EncryptionMeta) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.recordPart = append(c.recordPart, multipartPartCall{uploadID: uploadID, partNumber: partNumber, etag: etag, sizeBytes: size, enc: enc})
		return err
	}
}

func stubRecordObject(c *multipartCalls, err error) func(context.Context, string, string, int64, *core.EncryptionMeta) ([]core.DeletedCopy, error) {
	return func(_ context.Context, key, backend string, size int64, enc *core.EncryptionMeta) ([]core.DeletedCopy, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.recordObject = append(c.recordObject, multipartObjectCall{Key: key, Backend: backend, Size: size, Enc: enc})
		return nil, err
	}
}

func stubMultipartEnqueue(c *multipartCalls) func(context.Context, string, string, string, int64) error {
	return func(_ context.Context, backend, key, reason string, size int64) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.enqueue = append(c.enqueue, core.CleanupItem{
			BackendName: backend, ObjectKey: key, Reason: reason, SizeBytes: size,
		})
		return nil
	}
}

func stubIncrementOrphan(c *multipartCalls) func(context.Context, string, int64) error {
	return func(_ context.Context, backend string, size int64) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.incrementOrphan = append(c.incrementOrphan, orphanBytesEntry{backendName: backend, sizeBytes: size})
		return nil
	}
}

// multipartStubs wires the every-test default stubs onto store and
// returns the calls accumulator. Tests that need stricter expectations
// add EXPECT() calls themselves before calling Permissive() (which is
// the last setup step).
func multipartStubs(t *testing.T, store *storetest.MockMetadataStore) *multipartCalls {
	t.Helper()
	c := &multipartCalls{}
	store.EXPECT().CreateMultipartUpload(gomock.Any(), gomock.Any()).
		DoAndReturn(stubCreateMultipart(c, nil)).AnyTimes()
	store.EXPECT().DeleteMultipartUpload(gomock.Any(), gomock.Any()).
		DoAndReturn(stubDeleteMultipart(c)).AnyTimes()
	store.EXPECT().RecordPart(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubRecordPart(c, nil)).AnyTimes()
	store.EXPECT().RecordObject(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubRecordObject(c, nil)).AnyTimes()
	store.EXPECT().RecordObjectAndClearPending(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, key, backend string, size int64, enc *core.EncryptionMeta, _ string) ([]core.DeletedCopy, error) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.recordObject = append(c.recordObject, multipartObjectCall{Key: key, Backend: backend, Size: size, Enc: enc})
			return nil, nil
		}).AnyTimes()
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubMultipartEnqueue(c)).AnyTimes()
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubIncrementOrphan(c)).AnyTimes()
	return c
}

// TestCreateMultipartUpload_Success drives the happy path.
func TestCreateMultipartUpload_Success(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).
		Return("b1", nil).AnyTimes()
	multipartStubs(t, store)
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": backend})

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

// TestCreateMultipartUpload_DBUnavailable surfaces the DB-down branch.
func TestCreateMultipartUpload_DBUnavailable(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).
		Return("", core.ErrDBUnavailable).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	if _, _, err := mgr.MultipartManager.CreateMultipartUpload(context.Background(), "key", "", nil); !errors.Is(err, core.ErrServiceUnavailable) {
		t.Fatalf("expected st.ErrServiceUnavailable, got %v", err)
	}
}

// TestCreateMultipartUpload_NoSpace surfaces the no-space branch.
func TestCreateMultipartUpload_NoSpace(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).
		Return("", core.ErrNoSpaceAvailable).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	if _, _, err := mgr.MultipartManager.CreateMultipartUpload(context.Background(), "key", "", nil); !errors.Is(err, core.ErrInsufficientStorage) {
		t.Fatalf("expected st.ErrInsufficientStorage, got %v", err)
	}
}

// TestUploadPart_Success drives the happy path.
func TestUploadPart_Success(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
		Return(&core.MultipartUpload{
			UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1", ContentType: "application/zip",
		}, nil).AnyTimes()
	multipartStubs(t, store)
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": backend})

	etag, err := mgr.MultipartManager.UploadPart(context.Background(), "multi", "key", "upload-1", 1, bytes.NewReader([]byte("part-data")), 9)
	if err != nil {
		t.Fatalf("UploadPart: %v", err)
	}
	if etag == "" {
		t.Error("expected non-empty etag")
	}
	if !backend.hasObject("__multipart/upload-1/1") {
		t.Error("part not found on backend")
	}
}

// TestUploadPart_InvalidPartNumber rejects bogus part numbers.
func TestUploadPart_InvalidPartNumber(t *testing.T) {
	t.Parallel()
	mgr := newTestManager(t, newPermissiveMock(t), map[string]*mockBackend{"b1": newMockBackend()})

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

// TestUploadPart_DBUnavailable surfaces a metadata fetch failure.
func TestUploadPart_DBUnavailable(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
		Return(nil, core.ErrDBUnavailable).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	if _, err := mgr.MultipartManager.UploadPart(context.Background(), "multi", "key", "upload-1", 1, bytes.NewReader([]byte("x")), 1); !errors.Is(err, core.ErrServiceUnavailable) {
		t.Fatalf("expected st.ErrServiceUnavailable, got %v", err)
	}
}

// completeStoreSetup wires the GetMultipartUpload + GetParts stubs the
// CompleteMultipartUpload tests share.
func completeStoreSetup(t *testing.T, mu *core.MultipartUpload, parts []core.MultipartPart, partsErr error) (*storetest.MockMetadataStore, *multipartCalls) {
	t.Helper()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).Return(mu, nil).AnyTimes()
	if partsErr != nil {
		store.EXPECT().GetParts(gomock.Any(), gomock.Any()).Return(nil, partsErr).AnyTimes()
	} else {
		store.EXPECT().GetParts(gomock.Any(), gomock.Any()).Return(parts, nil).AnyTimes()
	}
	c := multipartStubs(t, store)
	storetest.Permissive(store)
	return store, c
}

// TestCompleteMultipartUpload_Success drives the assembly happy path.
func TestCompleteMultipartUpload_Success(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	ctx := context.Background()
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/2", bytes.NewReader([]byte("BBB")), 3, "application/octet-stream", nil)

	store, c := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1", ContentType: "application/zip"},
		[]core.MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3},
			{PartNumber: 2, ETag: "e2", SizeBytes: 3},
		}, nil)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": backend})

	etag, err := mgr.MultipartManager.CompleteMultipartUpload(ctx, "multi", "key", "upload-1", []int{1, 2})
	if err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}
	if etag == "" {
		t.Error("expected non-empty etag")
	}
	if !backend.hasObject("multi/key") {
		t.Error("final object not found on backend")
	}
	if backend.hasObject("__multipart/upload-1/1") || backend.hasObject("__multipart/upload-1/2") {
		t.Error("part temp keys should be deleted")
	}
	if len(c.recordObject) != 1 {
		t.Fatalf("expected 1 RecordObject call, got %d", len(c.recordObject))
	}
	call := c.recordObject[0]
	if call.Key != "multi/key" || call.Backend != "b1" || call.Size != 6 {
		t.Errorf("RecordObject called with %+v", call)
	}
}

// TestCompleteMultipartUpload_PopulatesContentHash pins issue #916:
// when integrity verification is enabled, CompleteMultipartUpload must
// record the assembled object with a content_hash matching SHA-256 of
// the assembled plaintext. Before the tee fix the recorded
// EncryptionMeta had no ContentHash, so multipart-completed objects
// were invisible to the scrubber.
func TestCompleteMultipartUpload_PopulatesContentHash(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	ctx := context.Background()
	_, _ = backend.PutObject(ctx, "__multipart/upload-h/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)
	_, _ = backend.PutObject(ctx, "__multipart/upload-h/2", bytes.NewReader([]byte("BBB")), 3, "application/octet-stream", nil)

	store, c := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-h", ObjectKey: "multi/hashed", BackendName: "b1", ContentType: "application/zip"},
		[]core.MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3},
			{PartNumber: 2, ETag: "e2", SizeBytes: 3},
		}, nil)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": backend})
	mgr.SetIntegrityConfig(&config.IntegrityConfig{Enabled: true})

	if _, err := mgr.MultipartManager.CompleteMultipartUpload(ctx, "multi", "hashed", "upload-h", []int{1, 2}); err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}
	if len(c.recordObject) != 1 {
		t.Fatalf("expected 1 RecordObject call, got %d", len(c.recordObject))
	}
	got := c.recordObject[0]
	if got.Enc == nil {
		t.Fatal("expected non-nil EncryptionMeta with ContentHash set")
	}

	want := sha256.Sum256([]byte("AAABBB"))
	wantHex := hex.EncodeToString(want[:])
	if got.Enc.ContentHash != wantHex {
		t.Errorf("ContentHash = %q, want %q", got.Enc.ContentHash, wantHex)
	}
}

// TestCompleteMultipartUpload_IntegrityDisabled_LeavesEncNil pins the
// inverse: with integrity disabled the recorded EncryptionMeta must
// stay nil so the no-encryption / no-integrity path keeps its existing
// (NULL content_hash) shape unchanged.
func TestCompleteMultipartUpload_IntegrityDisabled_LeavesEncNil(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	ctx := context.Background()
	_, _ = backend.PutObject(ctx, "__multipart/upload-d/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)

	store, c := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-d", ObjectKey: "multi/disabled", BackendName: "b1", ContentType: "application/zip"},
		[]core.MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3},
		}, nil)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": backend})
	// Integrity intentionally left unset.

	if _, err := mgr.MultipartManager.CompleteMultipartUpload(ctx, "multi", "disabled", "upload-d", []int{1}); err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}
	if got := c.recordObject[0]; got.Enc != nil {
		t.Errorf("Enc = %+v, want nil when integrity disabled", got.Enc)
	}
}

// TestCompleteMultipartUpload_DBUnavailable surfaces the DB-down branch.
func TestCompleteMultipartUpload_DBUnavailable(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
		Return(nil, core.ErrDBUnavailable).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	if _, err := mgr.MultipartManager.CompleteMultipartUpload(context.Background(), "multi", "key", "upload-1", []int{1}); !errors.Is(err, core.ErrServiceUnavailable) {
		t.Fatalf("expected st.ErrServiceUnavailable, got %v", err)
	}
}

// TestAbortMultipartUpload_Success drives the abort happy path.
func TestAbortMultipartUpload_Success(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	ctx := context.Background()
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)

	store, _ := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1"},
		[]core.MultipartPart{{PartNumber: 1, ETag: "e1", SizeBytes: 3, CreatedAt: time.Now()}}, nil)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": backend})

	if err := mgr.MultipartManager.AbortMultipartUpload(ctx, "multi", "key", "upload-1"); err != nil {
		t.Fatalf("AbortMultipartUpload: %v", err)
	}
	if backend.hasObject("__multipart/upload-1/1") {
		t.Error("part temp key should be deleted")
	}
	if got := mgr.Usage().Backend().Load("b1", counter.FieldAPIRequests); got != 2 {
		t.Errorf("apiRequests = %d, want 2 (1 part delete + 1 abort)", got)
	}
}

// TestAbortMultipartUpload_DBUnavailable surfaces the DB-down branch.
func TestAbortMultipartUpload_DBUnavailable(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
		Return(nil, core.ErrDBUnavailable).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	if err := mgr.MultipartManager.AbortMultipartUpload(context.Background(), "multi", "key", "upload-1"); !errors.Is(err, core.ErrServiceUnavailable) {
		t.Fatalf("expected st.ErrServiceUnavailable, got %v", err)
	}
}

// TestAbortMultipartUpload_GetPartsError surfaces the parts-fetch
// failure.
func TestAbortMultipartUpload_GetPartsError(t *testing.T) {
	t.Parallel()
	store, _ := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1"},
		nil, errors.New("db error"))
	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	if err := mgr.MultipartManager.AbortMultipartUpload(context.Background(), "multi", "key", "upload-1"); err == nil {
		t.Fatal("expected error from GetParts failure")
	}
}

// TestAbortMultipartUpload_PartDeleteFails_EnqueuesCleanup pins the
// orphan-cleanup branch when the backend delete fails during abort.
func TestAbortMultipartUpload_PartDeleteFails_EnqueuesCleanup(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	backend.delErr = errors.New("backend timeout")
	ctx := context.Background()
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)
	backend.mu.Lock()
	backend.delErr = errors.New("backend timeout")
	backend.mu.Unlock()

	store, c := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1"},
		[]core.MultipartPart{{PartNumber: 1, ETag: "e1", SizeBytes: 3}}, nil)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": backend})
	if err := mgr.MultipartManager.AbortMultipartUpload(ctx, "multi", "key", "upload-1"); err != nil {
		t.Fatalf("AbortMultipartUpload: %v", err)
	}

	if len(c.enqueue) != 1 {
		t.Fatalf("expected 1 enqueue call, got %d", len(c.enqueue))
	}
	if c.enqueue[0].Reason != "abort_part_cleanup" {
		t.Errorf("expected reason=abort_part_cleanup, got %q", c.enqueue[0].Reason)
	}
}

// TestCompleteMultipartUpload_PartSubset asserts the merge picks only
// the requested parts.
func TestCompleteMultipartUpload_PartSubset(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	ctx := context.Background()
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/2", bytes.NewReader([]byte("BBB")), 3, "application/octet-stream", nil)
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/3", bytes.NewReader([]byte("CCC")), 3, "application/octet-stream", nil)

	store, c := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1", ContentType: "application/zip"},
		[]core.MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3},
			{PartNumber: 2, ETag: "e2", SizeBytes: 3},
			{PartNumber: 3, ETag: "e3", SizeBytes: 3},
		}, nil)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": backend})

	etag, err := mgr.MultipartManager.CompleteMultipartUpload(ctx, "multi", "key", "upload-1", []int{1, 3})
	if err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}
	if etag == "" {
		t.Error("expected non-empty etag")
	}
	if !backend.hasObject("multi/key") {
		t.Fatal("final object not found on backend")
	}
	if len(c.recordObject) != 1 {
		t.Fatalf("expected 1 RecordObject call, got %d", len(c.recordObject))
	}
	if c.recordObject[0].Size != 6 {
		t.Errorf("expected recorded size=6, got %d", c.recordObject[0].Size)
	}
}

// TestCompleteMultipartUpload_InvalidPart asserts a missing part number
// surfaces InvalidPart.
func TestCompleteMultipartUpload_InvalidPart(t *testing.T) {
	t.Parallel()
	store, _ := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1", ContentType: "text/plain"},
		[]core.MultipartPart{{PartNumber: 1, ETag: "e1", SizeBytes: 3}}, nil)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	_, err := mgr.MultipartManager.CompleteMultipartUpload(context.Background(), "multi", "key", "upload-1", []int{1, 2})
	if err == nil {
		t.Fatal("expected error for missing part")
	}
	var s3err *core.S3Error
	if !errors.As(err, &s3err) || s3err.Code != "InvalidPart" {
		t.Errorf("expected st.S3Error with Code=InvalidPart, got %v", err)
	}
}

// TestCompleteMultipartUpload_LockContended pins the contended-lock
// 409 branch.
func TestCompleteMultipartUpload_LockContended(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
		Return(&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1"}, nil).AnyTimes()
	store.EXPECT().GetParts(gomock.Any(), gomock.Any()).
		Return([]core.MultipartPart{{PartNumber: 1, ETag: "e1", SizeBytes: 3}}, nil).AnyTimes()
	store.EXPECT().WithAdvisoryLock(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(false, nil).AnyTimes()
	c := multipartStubs(t, store)
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

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
	if len(c.recordObject) != 0 {
		t.Errorf("expected 0 RecordObject calls, got %d", len(c.recordObject))
	}
}

// TestCompleteMultipartUpload_AssemblyFails_CleansUpParts pins the
// deferred cleanup that fires on assembly PUT failure.
func TestCompleteMultipartUpload_AssemblyFails_CleansUpParts(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	ctx := context.Background()
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/2", bytes.NewReader([]byte("BBB")), 3, "application/octet-stream", nil)

	store, c := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1", ContentType: "application/zip"},
		[]core.MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3},
			{PartNumber: 2, ETag: "e2", SizeBytes: 3},
		}, nil)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": backend})

	backend.putErr = errors.New("backend write failed")

	_, err := mgr.MultipartManager.CompleteMultipartUpload(ctx, "multi", "key", "upload-1", []int{1, 2})
	if err == nil {
		t.Fatal("expected CompleteMultipartUpload to fail")
	}
	if backend.hasObject("__multipart/upload-1/1") || backend.hasObject("__multipart/upload-1/2") {
		t.Error("parts should have been deleted by deferred cleanup")
	}
	if !c.deleteMultipartHit {
		t.Error("expected DeleteMultipartUpload to be called by deferred cleanup")
	}
	if backend.hasObject("multi/key") {
		t.Error("assembled key should not exist when assembly PUT failed")
	}
}

// TestCompleteMultipartUpload_GetPartsError surfaces the parts-fetch
// failure.
func TestCompleteMultipartUpload_GetPartsError(t *testing.T) {
	t.Parallel()
	store, _ := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1"},
		nil, errors.New("db error"))

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	if _, err := mgr.MultipartManager.CompleteMultipartUpload(context.Background(), "multi", "key", "upload-1", []int{1}); err == nil {
		t.Fatal("expected error from GetParts failure")
	}
}

// TestCompleteMultipartUpload_PartDeleteFails_EnqueuesCleanup asserts a
// per-part delete failure enqueues a complete_part_cleanup row.
func TestCompleteMultipartUpload_PartDeleteFails_EnqueuesCleanup(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	ctx := context.Background()
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)

	store, c := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1", ContentType: "application/zip"},
		[]core.MultipartPart{{PartNumber: 1, ETag: "e1", SizeBytes: 3}}, nil)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": backend})

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
	if len(c.enqueue) != 1 {
		t.Fatalf("expected 1 enqueue call, got %d", len(c.enqueue))
	}
	if c.enqueue[0].Reason != "complete_part_cleanup" {
		t.Errorf("expected reason=complete_part_cleanup, got %q", c.enqueue[0].Reason)
	}
}

// TestCompleteMultipartUpload_FinalPutFails surfaces the assembly PUT
// failure.
func TestCompleteMultipartUpload_FinalPutFails(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	ctx := context.Background()
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)
	backend.putErr = errors.New("write failed")

	store, _ := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1", ContentType: "application/zip"},
		[]core.MultipartPart{{PartNumber: 1, ETag: "e1", SizeBytes: 3}}, nil)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": backend})
	if _, err := mgr.MultipartManager.CompleteMultipartUpload(ctx, "multi", "key", "upload-1", []int{1}); err == nil {
		t.Fatal("expected error when final PutObject fails")
	}
}

// TestCompleteMultipartUpload_PartReadFails surfaces a backend read
// failure.
func TestCompleteMultipartUpload_PartReadFails(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	ctx := context.Background()
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)
	backend.getReadErr = errors.New("disk I/O error")

	store, _ := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1", ContentType: "application/zip"},
		[]core.MultipartPart{{PartNumber: 1, ETag: "e1", SizeBytes: 3}}, nil)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": backend})
	if _, err := mgr.MultipartManager.CompleteMultipartUpload(ctx, "multi", "key", "upload-1", []int{1}); err == nil {
		t.Fatal("expected error when part body read fails")
	}
}

// TestUploadPart_UsageLimitExceeded surfaces the usage-limit guard.
func TestUploadPart_UsageLimitExceeded(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
		Return(&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1"}, nil).AnyTimes()
	multipartStubs(t, store)
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	mgr.Usage().UpdateLimits(map[string]core.UsageLimits{
		"b1": {IngressByteLimit: 1},
	})

	if _, err := mgr.MultipartManager.UploadPart(context.Background(), "multi", "key", "upload-1", 1, bytes.NewReader([]byte("large-data")), 10); !errors.Is(err, core.ErrInsufficientStorage) {
		t.Fatalf("expected st.ErrInsufficientStorage, got %v", err)
	}
}

// TestUploadPart_RecordPartFails_CleansUpPartObject pins the orphan-
// cleanup branch when RecordPart returns an error.
func TestUploadPart_RecordPartFails_CleansUpPartObject(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
		Return(&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1"}, nil).AnyTimes()
	store.EXPECT().RecordPart(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(errors.New("db error")).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": backend})

	if _, err := mgr.MultipartManager.UploadPart(context.Background(), "multi", "key", "upload-1", 1, bytes.NewReader([]byte("data")), 4); err == nil {
		t.Fatal("expected error from RecordPart failure")
	}
	if backend.hasObject("__multipart/upload-1/1") {
		t.Error("orphaned part should be deleted from backend")
	}
	if got := mgr.Usage().Backend().Load("b1", counter.FieldAPIRequests); got != 2 {
		t.Errorf("apiRequests = %d, want 2 (PUT + orphan DELETE)", got)
	}
}

// TestUploadPart_RecordPartFails_DeleteFails_EnqueuesCleanup asserts the
// fallback enqueue when both the record and the cleanup delete fail.
func TestUploadPart_RecordPartFails_DeleteFails_EnqueuesCleanup(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	backend.delErr = errors.New("backend timeout")

	c := &multipartCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
		Return(&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1"}, nil).AnyTimes()
	store.EXPECT().RecordPart(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(errors.New("db error")).AnyTimes()
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubMultipartEnqueue(c)).AnyTimes()
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubIncrementOrphan(c)).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": backend})

	if _, err := mgr.MultipartManager.UploadPart(context.Background(), "multi", "key", "upload-1", 1, bytes.NewReader([]byte("data")), 4); err == nil {
		t.Fatal("expected error from RecordPart failure")
	}

	if len(c.enqueue) != 1 {
		t.Fatalf("expected 1 enqueue call, got %d", len(c.enqueue))
	}
	if c.enqueue[0].Reason != "orphan_part_record_failed" {
		t.Errorf("reason = %q, want orphan_part_record_failed", c.enqueue[0].Reason)
	}
	if len(c.incrementOrphan) != 1 {
		t.Fatalf("expected 1 IncrementOrphanBytes call, got %d", len(c.incrementOrphan))
	}
	if c.incrementOrphan[0].sizeBytes != 4 {
		t.Errorf("orphan bytes = %d, want 4", c.incrementOrphan[0].sizeBytes)
	}
}

// TestCleanupStaleMultipartUploads_NoStaleUploads is a no-op smoke
// test.
func TestCleanupStaleMultipartUploads_NoStaleUploads(t *testing.T) {
	t.Parallel()
	mgr := newTestManager(t, newPermissiveMock(t), map[string]*mockBackend{"b1": newMockBackend()})
	mgr.MultipartManager.CleanupStaleMultipartUploads(context.Background(), time.Hour)
}

// TestCleanupStaleMultipartUploads_AbortFailureLogged returns a stale
// upload pointing at a backend that is not registered with the manager,
// so abortByMultipartRow's GetBackend call fails. The CleanupStaleMultipartUploads
// log path that reports the individual cleanup failure runs here.
func TestCleanupStaleMultipartUploads_AbortFailureLogged(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetStaleMultipartUploads(gomock.Any(), gomock.Any()).
		Return([]core.MultipartUpload{
			{UploadID: "abandoned-1", ObjectKey: "k", BackendName: "missing"},
		}, nil).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})
	mgr.MultipartManager.CleanupStaleMultipartUploads(context.Background(), time.Hour)
}

// TestAbortMultipartUploadsOnBackend_AbortFailureLogged drives the
// per-backend abort path where the upload's recorded backend is unknown,
// so abortByMultipartRow returns an error and the "failed to abort
// multipart upload" log fires.
func TestAbortMultipartUploadsOnBackend_AbortFailureLogged(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetMultipartUploadsByBackend(gomock.Any(), gomock.Any()).
		Return([]core.MultipartUpload{
			{UploadID: "abandoned-2", ObjectKey: "k", BackendName: "missing"},
		}, nil).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})
	mgr.MultipartManager.AbortMultipartUploadsOnBackend(context.Background(), "missing")
}

// TestCleanupStaleMultipartUploads_QueryError handles a stale-list
// query failure.
func TestCleanupStaleMultipartUploads_QueryError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetStaleMultipartUploads(gomock.Any(), gomock.Any()).
		Return(nil, errors.New("db error")).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})
	mgr.MultipartManager.CleanupStaleMultipartUploads(context.Background(), time.Hour)
}

// TestCleanupStaleMultipartUploads_AbortsStaleUploads pins the
// stale-abort happy path.
func TestCleanupStaleMultipartUploads_AbortsStaleUploads(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	ctx := context.Background()
	_, _ = backend.PutObject(ctx, "__multipart/stale-1/1", bytes.NewReader([]byte("x")), 1, "", nil)

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetStaleMultipartUploads(gomock.Any(), gomock.Any()).
		Return([]core.MultipartUpload{{UploadID: "stale-1", ObjectKey: "stale/key", BackendName: "b1"}}, nil).AnyTimes()
	store.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
		Return(&core.MultipartUpload{UploadID: "stale-1", ObjectKey: "stale/key", BackendName: "b1"}, nil).AnyTimes()
	store.EXPECT().GetParts(gomock.Any(), gomock.Any()).
		Return([]core.MultipartPart{{PartNumber: 1, ETag: "e1", SizeBytes: 1}}, nil).AnyTimes()
	multipartStubs(t, store)
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": backend})

	mgr.MultipartManager.CleanupStaleMultipartUploads(ctx, time.Hour)

	if backend.hasObject("__multipart/stale-1/1") {
		t.Error("stale part should be cleaned up")
	}
}

// TestCompleteMultipartUpload_UsageRecords2NPlus1APICalls pins the
// usage accounting (2N+1 calls).
func TestCompleteMultipartUpload_UsageRecords2NPlus1APICalls(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	ctx := context.Background()
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/2", bytes.NewReader([]byte("BBB")), 3, "application/octet-stream", nil)
	_, _ = backend.PutObject(ctx, "__multipart/upload-1/3", bytes.NewReader([]byte("CCC")), 3, "application/octet-stream", nil)

	store, _ := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1", ContentType: "application/zip"},
		[]core.MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3},
			{PartNumber: 2, ETag: "e2", SizeBytes: 3},
			{PartNumber: 3, ETag: "e3", SizeBytes: 3},
		}, nil)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": backend})
	if _, err := mgr.MultipartManager.CompleteMultipartUpload(ctx, "multi", "key", "upload-1", []int{1, 2, 3}); err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}

	wantAPICalls := int64(2*3 + 1)
	if got := mgr.Usage().Backend().Load("b1", counter.FieldAPIRequests); got != wantAPICalls {
		t.Errorf("apiRequests = %d, want %d (2*N+1 where N=3)", got, wantAPICalls)
	}
	if got := mgr.Usage().Backend().Load("b1", counter.FieldIngressBytes); got != 9 {
		t.Errorf("ingressBytes = %d, want 9", got)
	}
}

// TestUploadPart_BackendFailure_StillRecordsUsage pins a single API
// call is recorded even when the backend PUT fails.
func TestUploadPart_BackendFailure_StillRecordsUsage(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	backend.putErr = errors.New("backend timeout")

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
		Return(&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1"}, nil).AnyTimes()
	multipartStubs(t, store)
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": backend})

	if _, err := mgr.MultipartManager.UploadPart(context.Background(), "multi", "key", "upload-1", 1, bytes.NewReader([]byte("data")), 4); err == nil {
		t.Fatal("expected error from backend failure")
	}
	if got := mgr.Usage().Backend().Load("b1", counter.FieldAPIRequests); got != 1 {
		t.Errorf("apiRequests = %d, want 1 (failed call still counts)", got)
	}
	if got := mgr.Usage().Backend().Load("b1", counter.FieldIngressBytes); got != 0 {
		t.Errorf("ingressBytes = %d, want 0 (upload failed)", got)
	}
}

// TestCreateMultipartUpload_CreateStoreError surfaces the
// CreateMultipartUpload store-side failure.
func TestCreateMultipartUpload_CreateStoreError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).Return("b1", nil).AnyTimes()
	store.EXPECT().CreateMultipartUpload(gomock.Any(), gomock.Any()).
		Return(errors.New("db error")).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	if _, _, err := mgr.MultipartManager.CreateMultipartUpload(context.Background(), "key", "", nil); err == nil {
		t.Fatal("expected error from CreateMultipartUpload store failure")
	}
}

// TestCleanupStaleMultipartUploads_AbortFails handles an abort-side
// failure.
func TestCleanupStaleMultipartUploads_AbortFails(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetStaleMultipartUploads(gomock.Any(), gomock.Any()).
		Return([]core.MultipartUpload{{UploadID: "stale-1", ObjectKey: "stale/key", BackendName: "b1"}}, nil).AnyTimes()
	store.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
		Return(nil, errors.New("db error")).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	mgr.MultipartManager.CleanupStaleMultipartUploads(context.Background(), time.Hour)
}

// TestAbortMultipartUploadsOnBackend_ListError handles a list failure.
func TestAbortMultipartUploadsOnBackend_ListError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetMultipartUploadsByBackend(gomock.Any(), gomock.Any()).
		Return(nil, errors.New("db error")).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})
	mgr.MultipartManager.AbortMultipartUploadsOnBackend(context.Background(), "b1")
}

// TestAbortMultipartUploadsOnBackend_AbortsMatchingBackend pins the
// per-backend abort happy path.
func TestAbortMultipartUploadsOnBackend_AbortsMatchingBackend(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	ctx := context.Background()
	_, _ = backend.PutObject(ctx, "__multipart/up-1/1", bytes.NewReader([]byte("x")), 1, "", nil)

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetMultipartUploadsByBackend(gomock.Any(), gomock.Any()).
		Return([]core.MultipartUpload{
			{UploadID: "up-1", ObjectKey: "key1", BackendName: "b1"},
		}, nil).AnyTimes()
	store.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
		Return(&core.MultipartUpload{UploadID: "up-1", ObjectKey: "key1", BackendName: "b1"}, nil).AnyTimes()
	store.EXPECT().GetParts(gomock.Any(), gomock.Any()).
		Return([]core.MultipartPart{{PartNumber: 1, ETag: "e1", SizeBytes: 1}}, nil).AnyTimes()
	multipartStubs(t, store)
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": backend})

	mgr.MultipartManager.AbortMultipartUploadsOnBackend(ctx, "b1")

	if backend.hasObject("__multipart/up-1/1") {
		t.Error("stale part should be cleaned up")
	}
}

// TestAbortMultipartUploadsOnBackend_AbortFails handles a per-row
// abort failure.
func TestAbortMultipartUploadsOnBackend_AbortFails(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetMultipartUploadsByBackend(gomock.Any(), gomock.Any()).
		Return([]core.MultipartUpload{{UploadID: "up-1", ObjectKey: "key1", BackendName: "b1"}}, nil).AnyTimes()
	store.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
		Return(nil, errors.New("db error")).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})
	mgr.MultipartManager.AbortMultipartUploadsOnBackend(context.Background(), "b1")
}

// newEncryptedTestManager wires a manager with a real Encryptor so the
// shared-DEK code paths (unwrapUploadDEK, encryption-aware UploadPart,
// buildAssembledUpload) can be exercised in unit tests.
func newEncryptedTestManager(t *testing.T, store core.MetadataStore, backends map[string]*mockBackend) *BackendManager {
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
	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        obs,
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           order,
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
		Encryptor:       enc,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers
	return mgr
}

// failingKeyProvider always fails Wrap/Unwrap for the wrap-error tests.
type failingKeyProvider struct{}

func (failingKeyProvider) WrapDEK(_ context.Context, _ []byte) ([]byte, string, error) {
	return nil, "", errors.New("simulated wrap failure")
}

func (failingKeyProvider) UnwrapDEK(_ context.Context, _ []byte, _ string) ([]byte, error) {
	return nil, errors.New("simulated unwrap failure")
}

func (failingKeyProvider) KeyID() string { return "fail-0" }

// newFailingEncryptionTestManager wires a manager whose Encryptor's
// KeyProvider always fails.
func newFailingEncryptionTestManager(t *testing.T, store core.MetadataStore, backends map[string]*mockBackend) *BackendManager {
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
	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        obs,
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           order,
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
		Encryptor:       enc,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers
	return mgr
}

// TestCreateMultipartUpload_WrapDEKError surfaces the wrap-failure path.
func TestCreateMultipartUpload_WrapDEKError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).Return("b1", nil).AnyTimes()
	storetest.Permissive(store)

	mgr := newFailingEncryptionTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})
	if _, _, err := mgr.MultipartManager.CreateMultipartUpload(context.Background(), "k", "", nil); err == nil {
		t.Fatal("expected error from wrap failure, got nil")
	}
}

// TestCreateMultipartUpload_EncryptionWrapsSharedDEK pins the
// shared-DEK persistence on the upload row.
func TestCreateMultipartUpload_EncryptionWrapsSharedDEK(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).Return("b1", nil).AnyTimes()
	c := multipartStubs(t, store)
	storetest.Permissive(store)

	mgr := newEncryptedTestManager(t, store, map[string]*mockBackend{"b1": backend})
	if _, _, err := mgr.MultipartManager.CreateMultipartUpload(context.Background(), "k", "application/zip", nil); err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	if len(c.create) != 1 {
		t.Fatalf("expected 1 CreateMultipartUpload call, got %d", len(c.create))
	}
	got := c.create[0]
	if len(got.EncryptionKey) == 0 {
		t.Error("upload row missing wrapped EncryptionKey")
	}
	if got.KeyID == "" {
		t.Error("upload row missing KeyID")
	}
}

// TestUploadPart_ReusesSharedDEK pins the encryption-aware UploadPart
// branch.
func TestUploadPart_ReusesSharedDEK(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()

	var encKey []byte
	var keyID string
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).Return("b1", nil).AnyTimes()
	store.EXPECT().CreateMultipartUpload(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, params *core.CreateMultipartUploadParams) error {
			encKey = params.EncryptionKey
			keyID = params.KeyID
			return nil
		}).AnyTimes()
	store.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, uploadID string) (*core.MultipartUpload, error) {
			return &core.MultipartUpload{
				UploadID: uploadID, ObjectKey: "multi/k", BackendName: "b1",
				Encrypted: true, EncryptionKey: encKey, KeyID: keyID,
			}, nil
		}).AnyTimes()
	multipartStubs(t, store)
	storetest.Permissive(store)

	mgr := newEncryptedTestManager(t, store, map[string]*mockBackend{"b1": backend})

	uploadID, _, err := mgr.MultipartManager.CreateMultipartUpload(context.Background(), "multi/k", "", nil)
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	if _, err := mgr.MultipartManager.UploadPart(context.Background(), "multi", "k", uploadID, 1, bytes.NewReader([]byte("part-1-bytes")), 12); err != nil {
		t.Fatalf("UploadPart: %v", err)
	}
}

// TestCompleteMultipartUpload_Encrypted_RoundTrips pins the encrypted
// assembly path end-to-end.
func TestCompleteMultipartUpload_Encrypted_RoundTrips(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	ctx := context.Background()

	var encKey []byte
	var keyID string
	var partsCalls []multipartPartCall
	var partsCallsMu sync.Mutex
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).Return("b1", nil).AnyTimes()
	store.EXPECT().CreateMultipartUpload(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, params *core.CreateMultipartUploadParams) error {
			encKey = params.EncryptionKey
			keyID = params.KeyID
			return nil
		}).AnyTimes()
	store.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, uploadID string) (*core.MultipartUpload, error) {
			return &core.MultipartUpload{
				UploadID: uploadID, ObjectKey: "multi/k", BackendName: "b1",
				Encrypted: true, EncryptionKey: encKey, KeyID: keyID,
			}, nil
		}).AnyTimes()
	store.EXPECT().RecordPart(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, uploadID string, partNumber int, etag string, size int64, enc *core.EncryptionMeta) error {
			partsCallsMu.Lock()
			defer partsCallsMu.Unlock()
			partsCalls = append(partsCalls, multipartPartCall{uploadID: uploadID, partNumber: partNumber, etag: etag, sizeBytes: size, enc: enc})
			return nil
		}).AnyTimes()
	store.EXPECT().GetParts(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string) ([]core.MultipartPart, error) {
			partsCallsMu.Lock()
			defer partsCallsMu.Unlock()
			out := make([]core.MultipartPart, len(partsCalls))
			for i, pc := range partsCalls {
				out[i] = core.MultipartPart{
					PartNumber: pc.partNumber,
					ETag:       pc.etag,
					SizeBytes:  int64(backendObjectSize(backend, "__multipart/"+pc.uploadID+"/"+itoa(pc.partNumber))),
					Encrypted:  true,
					EncryptionKey: pc.enc.EncryptionKey,
					KeyID:         pc.enc.KeyID,
					PlaintextSize: 6,
				}
			}
			return out, nil
		}).AnyTimes()
	multipartStubs(t, store)
	storetest.Permissive(store)

	mgr := newEncryptedTestManager(t, store, map[string]*mockBackend{"b1": backend})

	uploadID, _, err := mgr.MultipartManager.CreateMultipartUpload(ctx, "multi/k", "", nil)
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	parts := [][]byte{[]byte("hello-"), []byte("world!")}
	for i, p := range parts {
		if _, err := mgr.MultipartManager.UploadPart(ctx, "multi", "k", uploadID, i+1, bytes.NewReader(p), int64(len(p))); err != nil {
			t.Fatalf("UploadPart %d: %v", i+1, err)
		}
	}
	if _, err := mgr.MultipartManager.CompleteMultipartUpload(ctx, "multi", "k", uploadID, []int{1, 2}); err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}
}

// itoa returns the base-10 string for a small int. Avoids importing
// strconv just for this single call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	negative := n < 0
	if negative {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// TestUnwrapUploadDEK_NoEncryptionMetadata covers the guardrail when
// the upload is unencrypted but unwrap is invoked.
func TestUnwrapUploadDEK_NoEncryptionMetadata(t *testing.T) {
	t.Parallel()
	mgr := newEncryptedTestManager(t, newPermissiveMock(t), map[string]*mockBackend{"b1": newMockBackend()})
	mu := &core.MultipartUpload{UploadID: "u1", Encrypted: false}
	if _, _, _, err := mgr.MultipartManager.UnwrapUploadDEK(context.Background(), mu); err == nil {
		t.Fatal("expected error for unencrypted upload, got nil")
	}
}

// TestUnwrapUploadDEK_UnpackError covers the unpack-error branch.
func TestUnwrapUploadDEK_UnpackError(t *testing.T) {
	t.Parallel()
	mgr := newEncryptedTestManager(t, newPermissiveMock(t), map[string]*mockBackend{"b1": newMockBackend()})
	mu := &core.MultipartUpload{
		UploadID: "u1", Encrypted: true,
		EncryptionKey: []byte{0x01, 0x02},
		KeyID:         "kid",
	}
	if _, _, _, err := mgr.MultipartManager.UnwrapUploadDEK(context.Background(), mu); err == nil {
		t.Fatal("expected error from UnpackKeyData, got nil")
	}
}

// TestUnwrapUploadDEK_UnwrapFails covers the keyprovider-rejection
// branch.
func TestUnwrapUploadDEK_UnwrapFails(t *testing.T) {
	t.Parallel()
	mgr := newEncryptedTestManager(t, newPermissiveMock(t), map[string]*mockBackend{"b1": newMockBackend()})
	bogus := encryption.PackKeyData(make([]byte, encryption.NonceSize), []byte("not-a-real-wrapped-dek"))
	mu := &core.MultipartUpload{UploadID: "u1", Encrypted: true, EncryptionKey: bogus, KeyID: "test-0"}
	if _, _, _, err := mgr.MultipartManager.UnwrapUploadDEK(context.Background(), mu); err == nil {
		t.Fatal("expected unwrap error, got nil")
	}
}

// TestUploadPart_UnwrapDEKError surfaces a wrap-key failure.
func TestUploadPart_UnwrapDEKError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
		Return(&core.MultipartUpload{
			UploadID: "u1", ObjectKey: "multi/k", BackendName: "b1",
			Encrypted:     true,
			EncryptionKey: encryption.PackKeyData(make([]byte, encryption.NonceSize), []byte("not-a-real-wrapped-dek")),
			KeyID:         "test-0",
		}, nil).AnyTimes()
	storetest.Permissive(store)

	mgr := newEncryptedTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})
	if _, err := mgr.MultipartManager.UploadPart(context.Background(), "multi", "k", "u1", 1, bytes.NewReader([]byte("data")), 4); err == nil {
		t.Fatal("expected unwrap error from UploadPart, got nil")
	}
}

// TestCompleteMultipartUpload_UnwrapDEKError surfaces a wrap-key
// failure during assembly.
func TestCompleteMultipartUpload_UnwrapDEKError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
		Return(&core.MultipartUpload{
			UploadID: "u1", ObjectKey: "multi/k", BackendName: "b1",
			Encrypted:     true,
			EncryptionKey: encryption.PackKeyData(make([]byte, encryption.NonceSize), []byte("not-a-real-wrapped-dek")),
			KeyID:         "test-0",
		}, nil).AnyTimes()
	store.EXPECT().GetParts(gomock.Any(), gomock.Any()).
		Return([]core.MultipartPart{{PartNumber: 1, ETag: "e", SizeBytes: 1, Encrypted: false}}, nil).AnyTimes()
	storetest.Permissive(store)

	mgr := newEncryptedTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})
	if _, err := mgr.MultipartManager.CompleteMultipartUpload(context.Background(), "multi", "k", "u1", []int{1}); err == nil {
		t.Fatal("expected unwrap error from Complete, got nil")
	}
}

// TestListMultipartUploads_PassThrough exercises the manager wrapper.
func TestListMultipartUploads_PassThrough(t *testing.T) {
	t.Parallel()
	want := []core.MultipartUpload{{UploadID: "u1"}, {UploadID: "u2"}}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListMultipartUploads(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(want, nil).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})
	got, err := mgr.MultipartManager.ListMultipartUploads(context.Background(), "p", 10)
	if err != nil {
		t.Fatalf("ListMultipartUploads: %v", err)
	}
	if len(got) != len(want) {
		t.Errorf("len = %d, want %d", len(got), len(want))
	}
}

// TestGetParts_PassThrough exercises the manager wrapper.
func TestGetParts_PassThrough(t *testing.T) {
	t.Parallel()
	want := []core.MultipartPart{{PartNumber: 1, ETag: "e1"}}
	store, _ := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "u1", ObjectKey: "multi/key", BackendName: "b1"},
		want, nil)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})
	got, err := mgr.MultipartManager.GetParts(context.Background(), "multi", "key", "u1")
	if err != nil {
		t.Fatalf("GetParts: %v", err)
	}
	if len(got) != 1 || got[0].PartNumber != 1 {
		t.Errorf("got = %+v, want %+v", got, want)
	}
}

// backendObjectSize returns the size of an object stored in a mock
// backend. Used by encryption tests that need to populate the parts
// list with realistic ciphertext sizes.
func backendObjectSize(b *mockBackend, key string) int {
	r, err := b.GetObject(context.Background(), key, "")
	if err != nil {
		return 0
	}
	defer r.Body.Close() //nolint:errcheck // best-effort close
	data, _ := io.ReadAll(r.Body)
	return len(data)
}

// TestCompleteMultipartUpload_PartGetPanics asserts a panic in the
// part-fetch goroutine surfaces as an error rather than deadlocking.
func TestCompleteMultipartUpload_PartGetPanics(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	backend.getPanic = true

	store, _ := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-panic", ObjectKey: "multi/panic", BackendName: "b1"},
		[]core.MultipartPart{{PartNumber: 1, ETag: "e1", SizeBytes: 3}}, nil)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": backend})
	if _, err := mgr.MultipartManager.CompleteMultipartUpload(context.Background(), "multi", "panic", "upload-panic", []int{1}); err == nil {
		t.Fatal("expected error from panicking part reader, got nil")
	}
}

// TestCompleteMultipartUpload_BackendTimeout pins #882: the final
// assembly PUT runs under backend_timeout. Pre-fix the PUT used the
// caller's request context directly, so a stalled backend during
// assembly could exceed backend_timeout.
func TestCompleteMultipartUpload_BackendTimeout(t *testing.T) {
	t.Parallel()
	be := newMockBackend()
	ctx := context.Background()
	_, _ = be.PutObject(ctx, "__multipart/upload-slow/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)
	_, _ = be.PutObject(ctx, "__multipart/upload-slow/2", bytes.NewReader([]byte("BBB")), 3, "application/octet-stream", nil)
	slow := &slowMockBackend{mockBackend: be, delay: 200 * time.Millisecond}

	store, _ := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-slow", ObjectKey: "multi/slow", BackendName: "b1", ContentType: "application/zip"},
		[]core.MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3},
			{PartNumber: 2, ETag: "e2", SizeBytes: 3},
		}, nil)

	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]s3be.ObjectBackend{"b1": slow},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"b1"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  50 * time.Millisecond,
		RoutingStrategy: config.RoutingPack,
	})

	_, err := mgr.MultipartManager.CompleteMultipartUpload(ctx, "multi", "slow", "upload-slow", []int{1, 2})
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
}
