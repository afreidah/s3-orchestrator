// -------------------------------------------------------------------------------
// Multipart Upload Tests
//
// Author: Alex Freidah
//
// Tests for the multipart upload lifecycle: CreateMultipartUpload,
// UploadPart, CompleteMultipartUpload, and AbortMultipartUpload. Validates
// backend delegation, metadata recording, and error handling.
// -------------------------------------------------------------------------------

package multipart

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

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// multipartCalls captures store interactions a multipart test wants to
// assert on.
type multipartCalls struct {
	mu                 sync.Mutex
	create             []core.CreateMultipartUploadParams
	deleteMultipartHit bool
	recordPart         []multipartPartCall
	recordObject       []multipartObjectCall
	enqueue            []core.CleanupItem
	incrementOrphan    []orphanBytesEntry
}

// orphanBytesEntry is one IncrementOrphanBytes call: which backend was
// charged and for how many bytes.
type orphanBytesEntry struct {
	backendName string
	sizeBytes   int64
}

type multipartPartCall struct {
	uploadID   string
	partNumber int
	etag       string
	sizeBytes  int64
	form       *core.StoredForm
}

type multipartObjectCall struct {
	Key, Backend string
	Size         int64
	Form         *core.StoredForm // pinned so tests can assert ContentHash etc.
	Tags         []core.Tag       // the set the create call carried, applied at complete
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

func stubRecordPart(c *multipartCalls, err error) func(context.Context, *core.RecordPartParams) error {
	return func(_ context.Context, p *core.RecordPartParams) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.recordPart = append(c.recordPart, multipartPartCall{
			uploadID: p.UploadID, partNumber: p.PartNumber, etag: p.ETag, sizeBytes: p.SizeBytes, form: p.Form,
		})
		return err
	}
}

func stubRecordObject(c *multipartCalls, err error) func(context.Context, *core.RecordObjectRequest) ([]core.DeletedCopy, core.QuotaDeltas, error) {
	return func(_ context.Context, req *core.RecordObjectRequest) ([]core.DeletedCopy, core.QuotaDeltas, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.recordObject = append(c.recordObject, multipartObjectCall{
			Key: req.Key, Backend: req.Backend, Size: req.Size, Form: req.Form, Tags: req.Tags,
		})
		return nil, core.QuotaDeltas{req.Backend: req.Size}, err
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
	store.EXPECT().RecordPart(gomock.Any(), gomock.Any()).
		DoAndReturn(stubRecordPart(c, nil)).AnyTimes()
	store.EXPECT().RecordObject(gomock.Any(), gomock.Any()).
		DoAndReturn(stubRecordObject(c, nil)).AnyTimes()
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubMultipartEnqueue(c)).AnyTimes()
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubIncrementOrphan(c)).AnyTimes()
	return c
}

// TestCreateMultipartUpload_Success drives the happy path.
func TestCreateMultipartUpload_Success(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	multipartStubs(t, store)
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	uploadID, backendName, err := mgr.CreateMultipartUpload(context.Background(), &CreateUploadRequest{Key: "multi/key", ContentType: "application/zip"})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	if uploadID == "" {
		t.Error("expected non-empty upload ID")
	}
	if backendName != "b1" {
		t.Errorf("be = %q, want %q", backendName, "b1")
	}
}

// TestCreateMultipartUpload_NoSpace surfaces the no-space branch. Selection is
// judged against the in-memory baseline, so a backend already at its limit is
// how a full fleet is expressed rather than a store error.
func TestCreateMultipartUpload_NoSpace(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	storetest.Permissive(store)

	// Past its limit rather than exactly at it: creating an upload claims no
	// bytes, so only a backend with negative headroom has nothing to offer.
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, &fleetOpts{
		QuotaBaselines: map[string]core.BackendQuotaUsage{
			"b1": {BackendName: "b1", BytesLimit: 100, BytesUsed: 150},
		},
	})

	if _, _, err := mgr.CreateMultipartUpload(context.Background(), &CreateUploadRequest{Key: "key"}); !errors.Is(err, core.ErrInsufficientStorage) {
		t.Fatalf("expected st.ErrInsufficientStorage, got %v", err)
	}
}

// TestUploadPart_Success drives the happy path.
func TestUploadPart_Success(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
		Return(&core.MultipartUpload{
			UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1", ContentType: "application/zip",
		}, nil).AnyTimes()
	multipartStubs(t, store)
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	etag, err := mgr.UploadPart(context.Background(), "multi", "key", "upload-1", 1, bytes.NewReader([]byte("part-data")), 9)
	if err != nil {
		t.Fatalf("UploadPart: %v", err)
	}
	if etag == "" {
		t.Error("expected non-empty etag")
	}
	if !be.Has("__multipart/upload-1/1") {
		t.Error("part not found on be")
	}
}

// TestUploadPart_InvalidPartNumber rejects bogus part numbers.
func TestUploadPart_InvalidPartNumber(t *testing.T) {
	t.Parallel()
	mgr := newFleet(t, newPermissiveStore(t), map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	for _, pn := range []int{0, -1, 10001, 1 << 20} {
		_, err := mgr.UploadPart(context.Background(), "multi", "key", "upload-1", pn, bytes.NewReader([]byte("x")), 1)
		if err == nil {
			t.Errorf("UploadPart(partNumber=%d) should fail", pn)
			continue
		}
		s3Err, ok := errors.AsType[*core.S3Error](err)
		if !ok || s3Err.Code != "InvalidArgument" {
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

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	if _, err := mgr.UploadPart(context.Background(), "multi", "key", "upload-1", 1, bytes.NewReader([]byte("x")), 1); !errors.Is(err, core.ErrServiceUnavailable) {
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
	be := backendtest.NewInMemory()
	ctx := context.Background()
	_, _ = be.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)
	_, _ = be.PutObject(ctx, "__multipart/upload-1/2", bytes.NewReader([]byte("BBB")), 3, "application/octet-stream", nil)

	store, c := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1", ContentType: "application/zip"},
		[]core.MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3},
			{PartNumber: 2, ETag: "e2", SizeBytes: 3},
		}, nil)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	etag, err := mgr.CompleteMultipartUpload(ctx, "multi", "key", "upload-1", partsOf(1, 2))
	if err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}
	if etag == "" {
		t.Error("expected non-empty etag")
	}
	if !be.Has("multi/key") {
		t.Error("final object not found on be")
	}
	if be.Has("__multipart/upload-1/1") || be.Has("__multipart/upload-1/2") {
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
// StoredForm had no ContentHash, so multipart-completed objects
// were invisible to the scrubber.
func TestCompleteMultipartUpload_PopulatesContentHash(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	ctx := context.Background()
	_, _ = be.PutObject(ctx, "__multipart/upload-h/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)
	_, _ = be.PutObject(ctx, "__multipart/upload-h/2", bytes.NewReader([]byte("BBB")), 3, "application/octet-stream", nil)

	store, c := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-h", ObjectKey: "multi/hashed", BackendName: "b1", ContentType: "application/zip"},
		[]core.MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3},
			{PartNumber: 2, ETag: "e2", SizeBytes: 3},
		}, nil)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	mgr.SetIntegrityConfig(&config.IntegrityConfig{Enabled: true})

	if _, err := mgr.CompleteMultipartUpload(ctx, "multi", "hashed", "upload-h", partsOf(1, 2)); err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}
	if len(c.recordObject) != 1 {
		t.Fatalf("expected 1 RecordObject call, got %d", len(c.recordObject))
	}
	got := c.recordObject[0]
	if got.Form == nil {
		t.Fatal("expected non-nil StoredForm with ContentHash set")
	}

	want := sha256.Sum256([]byte("AAABBB"))
	wantHex := hex.EncodeToString(want[:])
	if got.Form.ContentHash != wantHex {
		t.Errorf("ContentHash = %q, want %q", got.Form.ContentHash, wantHex)
	}
}

// TestCompleteMultipartUpload_IntegrityDisabled_LeavesFormNil pins the
// inverse: with integrity disabled the recorded StoredForm must
// stay nil so the no-encryption / no-integrity path keeps its existing
// (NULL content_hash) shape unchanged.
func TestCompleteMultipartUpload_IntegrityDisabled_LeavesFormNil(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	ctx := context.Background()
	_, _ = be.PutObject(ctx, "__multipart/upload-d/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)

	store, c := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-d", ObjectKey: "multi/disabled", BackendName: "b1", ContentType: "application/zip"},
		[]core.MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3},
		}, nil)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	// Integrity intentionally left unset.

	if _, err := mgr.CompleteMultipartUpload(ctx, "multi", "disabled", "upload-d", partsOf(1)); err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}
	if got := c.recordObject[0]; got.Form != nil {
		t.Errorf("Form = %+v, want nil when integrity disabled", got.Form)
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

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	if _, err := mgr.CompleteMultipartUpload(context.Background(), "multi", "key", "upload-1", partsOf(1)); !errors.Is(err, core.ErrServiceUnavailable) {
		t.Fatalf("expected st.ErrServiceUnavailable, got %v", err)
	}
}

// TestAbortMultipartUpload_Success drives the abort happy path.
func TestAbortMultipartUpload_Success(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	ctx := context.Background()
	_, _ = be.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)

	store, _ := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1"},
		[]core.MultipartPart{{PartNumber: 1, ETag: "e1", SizeBytes: 3, CreatedAt: time.Now()}}, nil)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	if err := mgr.AbortMultipartUpload(ctx, "multi", "key", "upload-1"); err != nil {
		t.Fatalf("AbortMultipartUpload: %v", err)
	}
	if be.Has("__multipart/upload-1/1") {
		t.Error("part temp key should be deleted")
	}
	if got := mgr.Runtime.Usage().Backend().Load("b1", counter.FieldAPIRequests); got != 2 {
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

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	if err := mgr.AbortMultipartUpload(context.Background(), "multi", "key", "upload-1"); !errors.Is(err, core.ErrServiceUnavailable) {
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
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	if err := mgr.AbortMultipartUpload(context.Background(), "multi", "key", "upload-1"); err == nil {
		t.Fatal("expected error from GetParts failure")
	}
}

// TestAbortMultipartUpload_PartDeleteFails_EnqueuesCleanup pins the
// orphan-cleanup branch when the be delete fails during abort.
func TestAbortMultipartUpload_PartDeleteFails_EnqueuesCleanup(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.DeleteErr = errors.New("be timeout")
	ctx := context.Background()
	_, _ = be.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)
	be.SetDeleteErr(errors.New("be timeout"))

	store, c := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1"},
		[]core.MultipartPart{{PartNumber: 1, ETag: "e1", SizeBytes: 3}}, nil)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	if err := mgr.AbortMultipartUpload(ctx, "multi", "key", "upload-1"); err != nil {
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
	be := backendtest.NewInMemory()
	ctx := context.Background()
	_, _ = be.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)
	_, _ = be.PutObject(ctx, "__multipart/upload-1/2", bytes.NewReader([]byte("BBB")), 3, "application/octet-stream", nil)
	_, _ = be.PutObject(ctx, "__multipart/upload-1/3", bytes.NewReader([]byte("CCC")), 3, "application/octet-stream", nil)

	store, c := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1", ContentType: "application/zip"},
		[]core.MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3},
			{PartNumber: 2, ETag: "e2", SizeBytes: 3},
			{PartNumber: 3, ETag: "e3", SizeBytes: 3},
		}, nil)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	etag, err := mgr.CompleteMultipartUpload(ctx, "multi", "key", "upload-1", partsOf(1, 3))
	if err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}
	if etag == "" {
		t.Error("expected non-empty etag")
	}
	if !be.Has("multi/key") {
		t.Fatal("final object not found on be")
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

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	_, err := mgr.CompleteMultipartUpload(context.Background(), "multi", "key", "upload-1", partsOf(1, 2))
	if err == nil {
		t.Fatal("expected error for missing part")
	}
	s3err, ok := errors.AsType[*core.S3Error](err)
	if !ok || s3err.Code != "InvalidPart" {
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

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	_, err := mgr.CompleteMultipartUpload(context.Background(), "multi", "key", "upload-1", partsOf(1))
	if err == nil {
		t.Fatal("expected OperationAborted error from contended lock")
	}
	s3err, ok := errors.AsType[*core.S3Error](err)
	if !ok {
		t.Fatalf("expected *core.S3Error, got %T: %v", err, err)
	}
	if s3err.StatusCode != 409 || s3err.Code != "OperationAborted" {
		t.Errorf("expected 409 OperationAborted, got %d %s", s3err.StatusCode, s3err.Code)
	}
	if len(c.recordObject) != 0 {
		t.Errorf("expected 0 RecordObject calls, got %d", len(c.recordObject))
	}
}

// TestCompleteMultipartUpload_AssemblyFails_PreservesParts pins the retry
// contract: a failed assembly PUT leaves every part and the upload row in
// place so the client can call Complete again. Destroying them here is what
// turned a transient backend fault into permanent data loss.
func TestCompleteMultipartUpload_AssemblyFails_PreservesParts(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	ctx := context.Background()
	_, _ = be.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)
	_, _ = be.PutObject(ctx, "__multipart/upload-1/2", bytes.NewReader([]byte("BBB")), 3, "application/octet-stream", nil)

	store, c := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1", ContentType: "application/zip"},
		[]core.MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3},
			{PartNumber: 2, ETag: "e2", SizeBytes: 3},
		}, nil)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	be.PutErr = errors.New("be write failed")

	_, err := mgr.CompleteMultipartUpload(ctx, "multi", "key", "upload-1", partsOf(1, 2))
	if err == nil {
		t.Fatal("expected CompleteMultipartUpload to fail")
	}
	if !be.Has("__multipart/upload-1/1") || !be.Has("__multipart/upload-1/2") {
		t.Error("parts must survive a failed assembly so the completion can be retried")
	}
	if c.deleteMultipartHit {
		t.Error("the upload row must survive a failed assembly")
	}
	if be.Has("multi/key") {
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

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	if _, err := mgr.CompleteMultipartUpload(context.Background(), "multi", "key", "upload-1", partsOf(1)); err == nil {
		t.Fatal("expected error from GetParts failure")
	}
}

// TestCompleteMultipartUpload_PartDeleteFails_EnqueuesCleanup asserts a
// per-part delete failure enqueues a complete_part_cleanup row.
func TestCompleteMultipartUpload_PartDeleteFails_EnqueuesCleanup(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	ctx := context.Background()
	_, _ = be.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)

	store, c := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1", ContentType: "application/zip"},
		[]core.MultipartPart{{PartNumber: 1, ETag: "e1", SizeBytes: 3}}, nil)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	be.SetDeleteErr(errors.New("be timeout"))

	etag, err := mgr.CompleteMultipartUpload(ctx, "multi", "key", "upload-1", partsOf(1))
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
	be := backendtest.NewInMemory()
	ctx := context.Background()
	_, _ = be.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)
	be.PutErr = errors.New("write failed")

	store, _ := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1", ContentType: "application/zip"},
		[]core.MultipartPart{{PartNumber: 1, ETag: "e1", SizeBytes: 3}}, nil)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	if _, err := mgr.CompleteMultipartUpload(ctx, "multi", "key", "upload-1", partsOf(1)); err == nil {
		t.Fatal("expected error when final PutObject fails")
	}
}

// TestCompleteMultipartUpload_PartReadFails surfaces a be read
// failure.
func TestCompleteMultipartUpload_PartReadFails(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	ctx := context.Background()
	_, _ = be.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)
	be.GetReadErr = errors.New("disk I/O error")

	store, _ := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1", ContentType: "application/zip"},
		[]core.MultipartPart{{PartNumber: 1, ETag: "e1", SizeBytes: 3}}, nil)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	if _, err := mgr.CompleteMultipartUpload(ctx, "multi", "key", "upload-1", partsOf(1)); err == nil {
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

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	mgr.Runtime.Usage().UpdateLimits(map[string]core.UsageLimits{
		"b1": {IngressByteLimit: 1},
	})

	if _, err := mgr.UploadPart(context.Background(), "multi", "key", "upload-1", 1, bytes.NewReader([]byte("large-data")), 10); !errors.Is(err, core.ErrInsufficientStorage) {
		t.Fatalf("expected st.ErrInsufficientStorage, got %v", err)
	}
}

// TestUploadPart_RecordPartFails_CleansUpPartObject pins the orphan-
// cleanup branch when RecordPart returns an error.
func TestUploadPart_RecordPartFails_CleansUpPartObject(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
		Return(&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1"}, nil).AnyTimes()
	store.EXPECT().RecordPart(gomock.Any(), gomock.Any()).
		Return(errors.New("db error")).AnyTimes()
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	if _, err := mgr.UploadPart(context.Background(), "multi", "key", "upload-1", 1, bytes.NewReader([]byte("data")), 4); err == nil {
		t.Fatal("expected error from RecordPart failure")
	}
	if be.Has("__multipart/upload-1/1") {
		t.Error("orphaned part should be deleted from be")
	}
	if got := mgr.Runtime.Usage().Backend().Load("b1", counter.FieldAPIRequests); got != 2 {
		t.Errorf("apiRequests = %d, want 2 (PUT + orphan DELETE)", got)
	}
}

// TestUploadPart_RecordPartFails_DeleteFails_EnqueuesCleanup asserts the
// fallback enqueue when both the record and the cleanup delete fail.
func TestUploadPart_RecordPartFails_DeleteFails_EnqueuesCleanup(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.DeleteErr = errors.New("be timeout")

	c := &multipartCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
		Return(&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1"}, nil).AnyTimes()
	store.EXPECT().RecordPart(gomock.Any(), gomock.Any()).
		Return(errors.New("db error")).AnyTimes()
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubMultipartEnqueue(c)).AnyTimes()
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubIncrementOrphan(c)).AnyTimes()
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	if _, err := mgr.UploadPart(context.Background(), "multi", "key", "upload-1", 1, bytes.NewReader([]byte("data")), 4); err == nil {
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
	mgr := newFleet(t, newPermissiveStore(t), map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	mgr.CleanupStaleMultipartUploads(context.Background(), time.Hour)
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

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	mgr.CleanupStaleMultipartUploads(context.Background(), time.Hour)
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

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	mgr.AbortMultipartUploadsOnBackend(context.Background(), "missing")
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

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	mgr.CleanupStaleMultipartUploads(context.Background(), time.Hour)
}

// TestCleanupStaleMultipartUploads_AbortsStaleUploads pins the
// stale-abort happy path.
func TestCleanupStaleMultipartUploads_AbortsStaleUploads(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	ctx := context.Background()
	_, _ = be.PutObject(ctx, "__multipart/stale-1/1", bytes.NewReader([]byte("x")), 1, "", nil)

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

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	mgr.CleanupStaleMultipartUploads(ctx, time.Hour)

	if be.Has("__multipart/stale-1/1") {
		t.Error("stale part should be cleaned up")
	}
}

// TestCompleteMultipartUpload_UsageRecords2NPlus1APICalls pins the
// usage accounting (2N+1 calls).
func TestCompleteMultipartUpload_UsageRecords2NPlus1APICalls(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	ctx := context.Background()
	_, _ = be.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)
	_, _ = be.PutObject(ctx, "__multipart/upload-1/2", bytes.NewReader([]byte("BBB")), 3, "application/octet-stream", nil)
	_, _ = be.PutObject(ctx, "__multipart/upload-1/3", bytes.NewReader([]byte("CCC")), 3, "application/octet-stream", nil)

	store, _ := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1", ContentType: "application/zip"},
		[]core.MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3},
			{PartNumber: 2, ETag: "e2", SizeBytes: 3},
			{PartNumber: 3, ETag: "e3", SizeBytes: 3},
		}, nil)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	if _, err := mgr.CompleteMultipartUpload(ctx, "multi", "key", "upload-1", partsOf(1, 2, 3)); err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}

	wantAPICalls := int64(2*3 + 1)
	if got := mgr.Runtime.Usage().Backend().Load("b1", counter.FieldAPIRequests); got != wantAPICalls {
		t.Errorf("apiRequests = %d, want %d (2*N+1 where N=3)", got, wantAPICalls)
	}
	if got := mgr.Runtime.Usage().Backend().Load("b1", counter.FieldIngressBytes); got != 9 {
		t.Errorf("ingressBytes = %d, want 9", got)
	}
}

// TestUploadPart_BackendFailure_StillRecordsUsage pins a single API
// call is recorded even when the be PUT fails.
func TestUploadPart_BackendFailure_StillRecordsUsage(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.PutErr = errors.New("be timeout")

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
		Return(&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1"}, nil).AnyTimes()
	multipartStubs(t, store)
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	if _, err := mgr.UploadPart(context.Background(), "multi", "key", "upload-1", 1, bytes.NewReader([]byte("data")), 4); err == nil {
		t.Fatal("expected error from be failure")
	}
	if got := mgr.Runtime.Usage().Backend().Load("b1", counter.FieldAPIRequests); got != 1 {
		t.Errorf("apiRequests = %d, want 1 (failed call still counts)", got)
	}
	if got := mgr.Runtime.Usage().Backend().Load("b1", counter.FieldIngressBytes); got != 0 {
		t.Errorf("ingressBytes = %d, want 0 (upload failed)", got)
	}
}

// TestCreateMultipartUpload_CreateStoreError surfaces the
// CreateMultipartUpload store-side failure.
func TestCreateMultipartUpload_CreateStoreError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().CreateMultipartUpload(gomock.Any(), gomock.Any()).
		Return(errors.New("db error")).AnyTimes()
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	if _, _, err := mgr.CreateMultipartUpload(context.Background(), &CreateUploadRequest{Key: "key"}); err == nil {
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

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	mgr.CleanupStaleMultipartUploads(context.Background(), time.Hour)
}

// TestAbortMultipartUploadsOnBackend_ListError handles a list failure.
func TestAbortMultipartUploadsOnBackend_ListError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetMultipartUploadsByBackend(gomock.Any(), gomock.Any()).
		Return(nil, errors.New("db error")).AnyTimes()
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	mgr.AbortMultipartUploadsOnBackend(context.Background(), "b1")
}

// TestAbortMultipartUploadsOnBackend_AbortsMatchingBackend pins the
// per-be abort happy path.
func TestAbortMultipartUploadsOnBackend_AbortsMatchingBackend(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	ctx := context.Background()
	_, _ = be.PutObject(ctx, "__multipart/up-1/1", bytes.NewReader([]byte("x")), 1, "", nil)

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

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	mgr.AbortMultipartUploadsOnBackend(ctx, "b1")

	if be.Has("__multipart/up-1/1") {
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

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	mgr.AbortMultipartUploadsOnBackend(context.Background(), "b1")
}

// newEncryptedTestManager wires a manager with a real Encryptor so the
// shared-DEK code paths (unwrapUploadDEK, encryption-aware UploadPart,
// buildAssembledUpload) can be exercised in unit tests.
func newEncryptedTestManager(t *testing.T, store storetest.MetadataStore, backends map[string]*backendtest.InMemory) *fleet {
	t.Helper()
	provider, err := encryption.NewConfigKeyProvider("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "test-0")
	if err != nil {
		t.Fatalf("NewConfigKeyProvider: %v", err)
	}
	enc, err := encryption.NewEncryptor(provider, 64*1024)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	obs := make(map[string]backend.ObjectBackend, len(backends))
	var order []string
	for name, b := range backends {
		obs[name] = b
		order = append(order, name)
	}
	mgr := newFleet(t, store, obs, &fleetOpts{Order: order, Encryptor: enc})
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
func newFailingEncryptionTestManager(t *testing.T, store storetest.MetadataStore, backends map[string]*backendtest.InMemory) *fleet {
	t.Helper()
	enc, err := encryption.NewEncryptor(failingKeyProvider{}, 64*1024)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	obs := make(map[string]backend.ObjectBackend, len(backends))
	var order []string
	for name, b := range backends {
		obs[name] = b
		order = append(order, name)
	}
	mgr := newFleet(t, store, obs, &fleetOpts{Order: order, Encryptor: enc})
	return mgr
}

// TestCreateMultipartUpload_WrapDEKError surfaces the wrap-failure path.
func TestCreateMultipartUpload_WrapDEKError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	storetest.Permissive(store)

	mgr := newFailingEncryptionTestManager(t, store, map[string]*backendtest.InMemory{"b1": backendtest.NewInMemory()})
	if _, _, err := mgr.CreateMultipartUpload(context.Background(), &CreateUploadRequest{Key: "k"}); err == nil {
		t.Fatal("expected error from wrap failure, got nil")
	}
}

// TestCreateMultipartUpload_EncryptionWrapsSharedDEK pins the
// shared-DEK persistence on the upload row.
func TestCreateMultipartUpload_EncryptionWrapsSharedDEK(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	c := multipartStubs(t, store)
	storetest.Permissive(store)

	mgr := newEncryptedTestManager(t, store, map[string]*backendtest.InMemory{"b1": be})
	if _, _, err := mgr.CreateMultipartUpload(context.Background(), &CreateUploadRequest{Key: "k", ContentType: "application/zip"}); err != nil {
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
	be := backendtest.NewInMemory()

	var encKey []byte
	var keyID string
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
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

	mgr := newEncryptedTestManager(t, store, map[string]*backendtest.InMemory{"b1": be})

	uploadID, _, err := mgr.CreateMultipartUpload(context.Background(), &CreateUploadRequest{Key: "multi/k"})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	if _, err := mgr.UploadPart(context.Background(), "multi", "k", uploadID, 1, bytes.NewReader([]byte("part-1-bytes")), 12); err != nil {
		t.Fatalf("UploadPart: %v", err)
	}
}

// TestCompleteMultipartUpload_Encrypted_RoundTrips pins the encrypted
// assembly path end-to-end.
func TestCompleteMultipartUpload_Encrypted_RoundTrips(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	ctx := context.Background()

	var encKey []byte
	var keyID string
	var partsCalls []multipartPartCall
	var partsCallsMu sync.Mutex
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
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
	store.EXPECT().RecordPart(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, p *core.RecordPartParams) error {
			partsCallsMu.Lock()
			defer partsCallsMu.Unlock()
			partsCalls = append(partsCalls, multipartPartCall{
				uploadID: p.UploadID, partNumber: p.PartNumber, etag: p.ETag, sizeBytes: p.SizeBytes, form: p.Form,
			})
			return nil
		}).AnyTimes()
	store.EXPECT().GetParts(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string) ([]core.MultipartPart, error) {
			partsCallsMu.Lock()
			defer partsCallsMu.Unlock()
			out := make([]core.MultipartPart, len(partsCalls))
			for i, pc := range partsCalls {
				out[i] = core.MultipartPart{
					PartNumber:    pc.partNumber,
					ETag:          pc.etag,
					SizeBytes:     int64(backendObjectSize(be, "__multipart/"+pc.uploadID+"/"+itoa(pc.partNumber))),
					Encrypted:     true,
					EncryptionKey: pc.form.EncryptionKey,
					KeyID:         pc.form.KeyID,
					PlaintextSize: 6,
				}
			}
			return out, nil
		}).AnyTimes()
	multipartStubs(t, store)
	storetest.Permissive(store)

	mgr := newEncryptedTestManager(t, store, map[string]*backendtest.InMemory{"b1": be})

	uploadID, _, err := mgr.CreateMultipartUpload(ctx, &CreateUploadRequest{Key: "multi/k"})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	parts := [][]byte{[]byte("hello-"), []byte("world!")}
	for i, p := range parts {
		if _, err := mgr.UploadPart(ctx, "multi", "k", uploadID, i+1, bytes.NewReader(p), int64(len(p))); err != nil {
			t.Fatalf("UploadPart %d: %v", i+1, err)
		}
	}
	if _, err := mgr.CompleteMultipartUpload(ctx, "multi", "k", uploadID, partsOf(1, 2)); err != nil {
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
	mgr := newEncryptedTestManager(t, newPermissiveStore(t), map[string]*backendtest.InMemory{"b1": backendtest.NewInMemory()})
	mu := &core.MultipartUpload{UploadID: "u1", Encrypted: false}
	if _, _, _, err := mgr.UnwrapUploadDEK(context.Background(), mu); err == nil {
		t.Fatal("expected error for unencrypted upload, got nil")
	}
}

// TestUnwrapUploadDEK_UnpackError covers the unpack-error branch.
func TestUnwrapUploadDEK_UnpackError(t *testing.T) {
	t.Parallel()
	mgr := newEncryptedTestManager(t, newPermissiveStore(t), map[string]*backendtest.InMemory{"b1": backendtest.NewInMemory()})
	mu := &core.MultipartUpload{
		UploadID: "u1", Encrypted: true,
		EncryptionKey: []byte{0x01, 0x02},
		KeyID:         "kid",
	}
	if _, _, _, err := mgr.UnwrapUploadDEK(context.Background(), mu); err == nil {
		t.Fatal("expected error from UnpackKeyData, got nil")
	}
}

// TestUnwrapUploadDEK_UnwrapFails covers the keyprovider-rejection
// branch.
func TestUnwrapUploadDEK_UnwrapFails(t *testing.T) {
	t.Parallel()
	mgr := newEncryptedTestManager(t, newPermissiveStore(t), map[string]*backendtest.InMemory{"b1": backendtest.NewInMemory()})
	bogus := encryption.PackKeyData(make([]byte, encryption.NonceSize), []byte("not-a-real-wrapped-dek"))
	mu := &core.MultipartUpload{UploadID: "u1", Encrypted: true, EncryptionKey: bogus, KeyID: "test-0"}
	if _, _, _, err := mgr.UnwrapUploadDEK(context.Background(), mu); err == nil {
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

	mgr := newEncryptedTestManager(t, store, map[string]*backendtest.InMemory{"b1": backendtest.NewInMemory()})
	if _, err := mgr.UploadPart(context.Background(), "multi", "k", "u1", 1, bytes.NewReader([]byte("data")), 4); err == nil {
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

	mgr := newEncryptedTestManager(t, store, map[string]*backendtest.InMemory{"b1": backendtest.NewInMemory()})
	if _, err := mgr.CompleteMultipartUpload(context.Background(), "multi", "k", "u1", partsOf(1)); err == nil {
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

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	got, err := mgr.ListMultipartUploads(context.Background(), "p", 10)
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

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	got, err := mgr.GetParts(context.Background(), "multi", "key", "u1")
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
func backendObjectSize(b *backendtest.InMemory, key string) int {
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
	be := backendtest.NewInMemory()
	be.GetPanic = true

	store, _ := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-panic", ObjectKey: "multi/panic", BackendName: "b1"},
		[]core.MultipartPart{{PartNumber: 1, ETag: "e1", SizeBytes: 3}}, nil)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	if _, err := mgr.CompleteMultipartUpload(context.Background(), "multi", "panic", "upload-panic", partsOf(1)); err == nil {
		t.Fatal("expected error from panicking part reader, got nil")
	}
}

// TestCompleteMultipartUpload_BackendTimeout pins #882: the final
// assembly PUT runs under backend_timeout. Pre-fix the PUT used the
// caller's request context directly, so a stalled backend during
// assembly could exceed backend_timeout.
func TestCompleteMultipartUpload_BackendTimeout(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	ctx := context.Background()
	_, _ = be.PutObject(ctx, "__multipart/upload-slow/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)
	_, _ = be.PutObject(ctx, "__multipart/upload-slow/2", bytes.NewReader([]byte("BBB")), 3, "application/octet-stream", nil)
	slow := backendtest.NewSlow(be, 200*time.Millisecond)

	store, _ := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-slow", ObjectKey: "multi/slow", BackendName: "b1", ContentType: "application/zip"},
		[]core.MultipartPart{
			{PartNumber: 1, ETag: "e1", SizeBytes: 3},
			{PartNumber: 2, ETag: "e2", SizeBytes: 3},
		}, nil)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": slow},
		&fleetOpts{Order: []string{"b1"}, BackendTimeout: 50 * time.Millisecond})

	_, err := mgr.CompleteMultipartUpload(ctx, "multi", "slow", "upload-slow", partsOf(1, 2))
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
}

// -------------------------------------------------------------------------
// COMPLETION RETRY SAFETY
// -------------------------------------------------------------------------

// twoPartUpload seeds a backend with two part objects and returns it with the
// matching stored rows, which is the fixture every retry-safety case needs.
func twoPartUpload(t *testing.T) (*backendtest.InMemory, []core.MultipartPart) {
	t.Helper()
	be := backendtest.NewInMemory()
	ctx := context.Background()
	_, _ = be.PutObject(ctx, "__multipart/upload-1/1", bytes.NewReader([]byte("AAA")), 3, "application/octet-stream", nil)
	_, _ = be.PutObject(ctx, "__multipart/upload-1/2", bytes.NewReader([]byte("BBB")), 3, "application/octet-stream", nil)
	return be, []core.MultipartPart{
		{PartNumber: 1, ETag: "e1", SizeBytes: 3},
		{PartNumber: 2, ETag: "e2", SizeBytes: 3},
	}
}

// assertUploadRetryable fails the test unless both part objects and the
// upload row survived, which is the precondition for a client retry.
func assertUploadRetryable(t *testing.T, be *backendtest.InMemory, c *multipartCalls) {
	t.Helper()
	if !be.Has("__multipart/upload-1/1") || !be.Has("__multipart/upload-1/2") {
		t.Error("parts were destroyed; the completion is no longer retryable")
	}
	if c.deleteMultipartHit {
		t.Error("upload row was deleted; the completion is no longer retryable")
	}
}

// TestCompleteMultipartUpload_StreamFailure_PreservesParts covers a part that
// disappears mid-assembly: the stream fails, and the remaining parts and the
// upload row must survive for the retry.
func TestCompleteMultipartUpload_StreamFailure_PreservesParts(t *testing.T) {
	t.Parallel()
	be, parts := twoPartUpload(t)
	store, c := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1"},
		parts, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	be.SetGetErr(errors.New("part read failed"))

	if _, err := mgr.CompleteMultipartUpload(context.Background(), "multi", "key", "upload-1", partsOf(1, 2)); err == nil {
		t.Fatal("expected the completion to fail when a part cannot be read")
	}
	assertUploadRetryable(t, be, c)
}

// TestCompleteMultipartUpload_CommitFailure_PreservesParts covers the
// metadata commit failing after the bytes land. The assembled object is
// cleaned up by the write coordinator, and the parts stay for the retry.
func TestCompleteMultipartUpload_CommitFailure_PreservesParts(t *testing.T) {
	t.Parallel()
	be, parts := twoPartUpload(t)

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
		Return(&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1"}, nil).AnyTimes()
	store.EXPECT().GetParts(gomock.Any(), gomock.Any()).Return(parts, nil).AnyTimes()
	// The commit is what fails here, after the assembly PUT has succeeded.
	// Registered before multipartStubs so it wins: gomock matches in
	// declaration order and multipartStubs stubs RecordObject as succeeding.
	store.EXPECT().RecordObject(gomock.Any(), gomock.Any()).
		Return(nil, nil, errors.New("db down")).AnyTimes()
	c := multipartStubs(t, store)
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	if _, err := mgr.CompleteMultipartUpload(context.Background(), "multi", "key", "upload-1", partsOf(1, 2)); err == nil {
		t.Fatal("expected the completion to fail when the metadata commit fails")
	}
	assertUploadRetryable(t, be, c)
}

// TestCompleteMultipartUpload_RetryAfterTransientFailure is the contract
// #1164 exists for: a completion that failed on a transient backend error
// succeeds when the client retries it.
func TestCompleteMultipartUpload_RetryAfterTransientFailure(t *testing.T) {
	t.Parallel()
	be, parts := twoPartUpload(t)
	store, c := completeStoreSetup(t,
		&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1"},
		parts, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	be.SetPutErr(errors.New("transient backend timeout"))
	if _, err := mgr.CompleteMultipartUpload(context.Background(), "multi", "key", "upload-1", partsOf(1, 2)); err == nil {
		t.Fatal("expected the first completion to fail")
	}
	assertUploadRetryable(t, be, c)

	be.SetPutErr(nil)
	if _, err := mgr.CompleteMultipartUpload(context.Background(), "multi", "key", "upload-1", partsOf(1, 2)); err != nil {
		t.Fatalf("retry after a transient failure should succeed: %v", err)
	}
	if !be.Has("multi/key") {
		t.Error("assembled object missing after a successful retry")
	}
	if len(c.recordObject) != 1 {
		t.Errorf("RecordObject calls = %d, want 1", len(c.recordObject))
	}
	// Only the successful attempt retires the parts.
	if be.Has("__multipart/upload-1/1") || be.Has("__multipart/upload-1/2") {
		t.Error("parts should be dropped once the object is durably committed")
	}
	if !c.deleteMultipartHit {
		t.Error("upload row should be dropped once the object is durably committed")
	}
}

// TestCompleteMultipartUpload_InvalidManifest_PreservesParts asserts a
// rejected manifest starts no assembly and leaves the upload retryable, so a
// client can correct the request and try again.
func TestCompleteMultipartUpload_InvalidManifest_PreservesParts(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		manifest []core.CompletePart
	}{
		{"descending", []core.CompletePart{{PartNumber: 2}, {PartNumber: 1}}},
		{"duplicate", []core.CompletePart{{PartNumber: 1}, {PartNumber: 1}}},
		{"out of range", []core.CompletePart{{PartNumber: MaxPartNumber + 1}}},
		{"stale etag", []core.CompletePart{{PartNumber: 1, ETag: "wrong"}, {PartNumber: 2, ETag: "e2"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			be, parts := twoPartUpload(t)
			store, calls := completeStoreSetup(t,
				&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1"},
				parts, nil)
			mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

			if _, err := mgr.CompleteMultipartUpload(context.Background(), "multi", "key", "upload-1", c.manifest); err == nil {
				t.Fatal("expected the completion to be rejected")
			}
			if be.Has("multi/key") {
				t.Error("no assembly should start for a rejected manifest")
			}
			if len(calls.recordObject) != 0 {
				t.Error("no commit should happen for a rejected manifest")
			}
			assertUploadRetryable(t, be, calls)
		})
	}
}

// TestCompleteMultipartUpload_EnforcesMinPartSize pins the 5 MiB floor and
// the operator switch that turns it off.
func TestCompleteMultipartUpload_EnforcesMinPartSize(t *testing.T) {
	t.Parallel()
	// Both parts are small, so the first one violates the floor.
	small := []core.MultipartPart{
		{PartNumber: 1, ETag: "e1", SizeBytes: 3},
		{PartNumber: 2, ETag: "e2", SizeBytes: 3},
	}

	t.Run("rejected when enforced", func(t *testing.T) {
		t.Parallel()
		be, _ := twoPartUpload(t)
		store, _ := completeStoreSetup(t,
			&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1"},
			small, nil)
		mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be},
			&fleetOpts{EnforceMinPartSize: true})

		_, err := mgr.CompleteMultipartUpload(context.Background(), "multi", "key", "upload-1", partsOf(1, 2))
		if got := s3CodeOf(t, err); got != "EntityTooSmall" {
			t.Errorf("code = %s, want EntityTooSmall", got)
		}
	})

	t.Run("accepted when the floor is off", func(t *testing.T) {
		t.Parallel()
		be, _ := twoPartUpload(t)
		store, _ := completeStoreSetup(t,
			&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1"},
			small, nil)
		mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

		if _, err := mgr.CompleteMultipartUpload(context.Background(), "multi", "key", "upload-1", partsOf(1, 2)); err != nil {
			t.Fatalf("small parts should be accepted when the floor is off: %v", err)
		}
	})
}
