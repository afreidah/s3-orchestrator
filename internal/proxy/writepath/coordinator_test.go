// -------------------------------------------------------------------------------
// Write Coordinator - Branch Tests
//
// Author: Alex Freidah
//
// Targeted tests for the branches of Coordinator that the existing
// PUT/multipart end-to-end tests do not exercise: encryption metadata
// copy on InsertPendingIntent, InsertPending error propagation, and the
// "backend not registered" branch of RecordObjectAndPromoteIntent when
// intentID is empty.
// -------------------------------------------------------------------------------

package writepath

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/mock/gomock"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// httpError is a writepath-package test helper that satisfies the
// HTTPStatusCode() interface backend.IsNotFound classifies against.
type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string       { return e.msg }
func (e *httpError) HTTPStatusCode() int { return e.code }

// newCoordinatorWithBackend builds a Coordinator whose infra.BackendRuntime knows
// about a single named backend. Used by DeleteOrEnqueue branch tests so
// the backend's DeleteObject can be controlled directly through the
// gomock recorder. A real (in-memory) UsageTracker is supplied so the
// Acct().APICall path on DeleteOrEnqueue does not nil-deref.
func newCoordinatorWithBackend(name string, be s3be.ObjectBackend, store CoordinatorStores) *Coordinator {
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{name}), nil)
	c := infra.New(&infra.Config{
		Backends: map[string]s3be.ObjectBackend{name: be},
		Usage:    usage,
	})
	return New(c, store, true)
}

// newCoordinatorWithStore builds a minimal Coordinator backed by the
// supplied store. Avoids the BackendManager constructor so coordinator
// branches can be exercised in isolation without dragging the full
// manager assembly into every test.
func newCoordinatorWithStore(store CoordinatorStores, pendingEnabled bool) *Coordinator {
	c := infra.New(&infra.Config{
		Backends: map[string]s3be.ObjectBackend{},
	})
	return New(c, store, pendingEnabled)
}

// newCoordinatorWith2Backends builds a Coordinator that knows a source and a
// destination backend, so MoveObject's StreamCopy (GetObject src -> PutObject
// dest) and its cleanup paths can be driven through the gomock recorders.
func newCoordinatorWith2Backends(srcName string, src s3be.ObjectBackend, destName string, dest s3be.ObjectBackend, store CoordinatorStores) *Coordinator {
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{srcName, destName}), nil)
	c := infra.New(&infra.Config{
		Backends:       map[string]s3be.ObjectBackend{srcName: src, destName: dest},
		BackendTimeout: 5 * time.Second,
		Usage:          usage,
	})
	return New(c, store, true)
}

// expectStreamCopyOK wires the happy StreamCopy: read 4 bytes from src, write
// them to dest.
func expectStreamCopyOK(src, dest *backendtest.MockObjectBackend) {
	src.EXPECT().GetObject(gomock.Any(), "k", "").
		Return(&s3be.GetObjectResult{Body: io.NopCloser(strings.NewReader("data")), Size: 4}, nil)
	dest.EXPECT().PutObject(gomock.Any(), "k", gomock.Any(), int64(4), gomock.Any(), gomock.Any()).
		Return("etag", nil)
}

// TestMoveObject_CASError_OrphansDestWithProfileReason pins the metadata-CAS
// failure path: the destination bytes are orphaned with the profile's Orphan
// reason.
func TestMoveObject_CASError_OrphansDestWithProfileReason(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	src := backendtest.NewMockObjectBackend(ctrl)
	dest := backendtest.NewMockObjectBackend(ctrl)
	expectStreamCopyOK(src, dest)
	// CAS errors -> orphan cleanup on dest; force the enqueue by failing the delete.
	dest.EXPECT().DeleteObject(gomock.Any(), "k").Return(errors.New("delete failed"))

	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().MoveObjectLocation(gomock.Any(), "k", "src", "dest").Return(int64(0), errors.New("cas failed"))
	store.EXPECT().EnqueueCleanup(gomock.Any(), "dest", "k", RebalanceMoveReasons.Orphan, int64(4)).Return(nil).Times(1)
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), "dest", int64(4)).Return(nil).Times(1)

	coord := newCoordinatorWith2Backends("src", src, "dest", dest, store)
	if _, err := coord.MoveObject(context.Background(), moveReq(src, dest)); err == nil {
		t.Fatal("expected error on CAS failure")
	}
}

// TestMoveObject_Stale_StaleOrphansDestWithProfileReason pins the raced-row
// path (movedSize=0): the destination bytes are orphaned with the StaleOrphan
// reason and the call returns ErrMoveStale.
func TestMoveObject_Stale_StaleOrphansDestWithProfileReason(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	src := backendtest.NewMockObjectBackend(ctrl)
	dest := backendtest.NewMockObjectBackend(ctrl)
	expectStreamCopyOK(src, dest)
	dest.EXPECT().DeleteObject(gomock.Any(), "k").Return(errors.New("delete failed"))

	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().MoveObjectLocation(gomock.Any(), "k", "src", "dest").Return(int64(0), nil)
	store.EXPECT().EnqueueCleanup(gomock.Any(), "dest", "k", RebalanceMoveReasons.StaleOrphan, int64(4)).Return(nil).Times(1)
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), "dest", int64(4)).Return(nil).Times(1)

	coord := newCoordinatorWith2Backends("src", src, "dest", dest, store)
	if _, err := coord.MoveObject(context.Background(), moveReq(src, dest)); !errors.Is(err, ErrMoveStale) {
		t.Fatalf("err = %v, want ErrMoveStale", err)
	}
}

// TestMoveObject_Success_SourceDeleteWithProfileReason pins the success path:
// the source copy is deleted with the SourceDelete reason and the authoritative
// moved size is returned.
func TestMoveObject_Success_SourceDeleteWithProfileReason(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	src := backendtest.NewMockObjectBackend(ctrl)
	dest := backendtest.NewMockObjectBackend(ctrl)
	expectStreamCopyOK(src, dest)
	// Success -> source delete; force the enqueue by failing the delete so the
	// SourceDelete reason is observable on EnqueueCleanup.
	src.EXPECT().DeleteObject(gomock.Any(), "k").Return(errors.New("delete failed"))

	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().MoveObjectLocation(gomock.Any(), "k", "src", "dest").Return(int64(4), nil)
	store.EXPECT().EnqueueCleanup(gomock.Any(), "src", "k", RebalanceMoveReasons.SourceDelete, int64(4)).Return(nil).Times(1)
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), "src", int64(4)).Return(nil).Times(1)

	coord := newCoordinatorWith2Backends("src", src, "dest", dest, store)
	movedSize, err := coord.MoveObject(context.Background(), moveReq(src, dest))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if movedSize != 4 {
		t.Errorf("movedSize = %d, want 4", movedSize)
	}
}

// moveReq builds the standard rebalance MoveRequest the MoveObject tests share.
func moveReq(src, dest s3be.ObjectBackend) *MoveRequest {
	return &MoveRequest{
		Key:         "k",
		SizeBytes:   4,
		SrcBackend:  src,
		SrcName:     "src",
		DestBackend: dest,
		DestName:    "dest",
		Reasons:     RebalanceMoveReasons,
	}
}

// TestInsertPendingIntent_CopiesStoredForm drives the form != nil
// branch so the PendingObject is populated with the wrapped DEK,
// keyID, plaintext size, and content hash.
func TestInsertPendingIntent_CopiesStoredForm(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockCoordinatorStores(ctrl)

	var got core.PendingObject
	store.EXPECT().InsertPending(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, p *core.PendingObject) error {
			got = *p
			return nil
		}).Times(1)

	coord := newCoordinatorWithStore(store, true)
	form := &core.StoredForm{
		Encrypted:     true,
		EncryptionKey: []byte("wrapped-dek-bytes"),
		KeyID:         "kid-1",
		PlaintextSize: 4096,
		ContentHash:   "deadbeef",
	}

	intentID, err := coord.InsertPendingIntent(context.Background(), "k", "b1", 4096, form)
	if err != nil {
		t.Fatalf("InsertPendingIntent: %v", err)
	}
	if intentID == "" {
		t.Fatal("expected non-empty intentID")
	}
	if !got.Encrypted || got.KeyID != "kid-1" || got.PlaintextSize != 4096 || got.ContentHash != "deadbeef" {
		t.Errorf("encryption metadata not copied onto PendingObject: %+v", got)
	}
	if string(got.EncryptionKey) != "wrapped-dek-bytes" {
		t.Errorf("EncryptionKey not copied: %q", got.EncryptionKey)
	}
}

// TestInsertPendingIntent_CopiesCompression pins the rest of the description an
// intent has to carry. The intent is the only record of how the bytes were
// written until the commit lands, so a field missing here is a crash-recovered
// object whose row says verbatim over an encoding nothing then decodes.
//
// SizeBytes is asserted alongside because it is what quota is reconciled
// against on recovery: the bytes that occupy the backend, not the larger object
// they decode to.
func TestInsertPendingIntent_CopiesCompression(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockCoordinatorStores(ctrl)

	var got core.PendingObject
	store.EXPECT().InsertPending(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, p *core.PendingObject) error {
			got = *p
			return nil
		}).Times(1)

	coord := newCoordinatorWithStore(store, true)
	form := &core.StoredForm{
		ContentHash:              "deadbeef",
		CompressionAlgorithm:     "zstd",
		CompressionLevel:         "default",
		CompressionFormatVersion: 1,
		LogicalSize:              8192,
	}

	if _, err := coord.InsertPendingIntent(context.Background(), "k", "b1", 4096, form); err != nil {
		t.Fatalf("InsertPendingIntent: %v", err)
	}
	if got.CompressionAlgorithm != "zstd" {
		t.Errorf("CompressionAlgorithm = %q, want %q", got.CompressionAlgorithm, "zstd")
	}
	if got.CompressionLevel != "default" {
		t.Errorf("CompressionLevel = %q, want %q", got.CompressionLevel, "default")
	}
	if got.CompressionFormatVersion != 1 {
		t.Errorf("CompressionFormatVersion = %d, want 1", got.CompressionFormatVersion)
	}
	if got.LogicalSize != 8192 {
		t.Errorf("LogicalSize = %d, want 8192", got.LogicalSize)
	}
	if got.SizeBytes != 4096 {
		t.Errorf("SizeBytes = %d, want the 4096 landing on the backend", got.SizeBytes)
	}
}

// TestInsertPendingIntent_StoreError covers the InsertPending failure
// branch: the wrapped error is returned and the intent ID is empty.
func TestInsertPendingIntent_StoreError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().InsertPending(gomock.Any(), gomock.Any()).
		Return(errors.New("db down")).Times(1)

	coord := newCoordinatorWithStore(store, true)

	intentID, err := coord.InsertPendingIntent(context.Background(), "k", "b1", 4096, nil)
	if err == nil {
		t.Fatal("expected error from InsertPending failure")
	}
	if intentID != "" {
		t.Errorf("expected empty intentID on error, got %q", intentID)
	}
}

// TestDeleteOrEnqueue_NotFound_SkipsEnqueue asserts that when the
// immediate backend DELETE returns 404, no cleanup_queue row is inserted
// (the backend already agrees the object is gone). Issue #843 - this
// prevents phantom 404s from seeding the cleanup queue at the source.
func TestDeleteOrEnqueue_NotFound_SkipsEnqueue(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	be := backendtest.NewMockObjectBackend(ctrl)
	be.EXPECT().DeleteObject(gomock.Any(), "phantom.txt").
		Return(&httpError{code: 404, msg: "NoSuchKey"})

	store := NewMockCoordinatorStores(ctrl)
	// The whole point of the fix: EnqueueCleanup must NOT be called.
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

	coord := newCoordinatorWithBackend("b1", be, store)
	coord.DeleteOrEnqueue(context.Background(), be, "b1", "phantom.txt", "overwrite_displaced", 128)
}

// TestDeleteOrEnqueue_GenericError_Enqueues asserts the regression
// guard for the existing failure path: a non-404 error still enqueues
// the cleanup so the worker can retry.
func TestDeleteOrEnqueue_GenericError_Enqueues(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	be := backendtest.NewMockObjectBackend(ctrl)
	be.EXPECT().DeleteObject(gomock.Any(), "real.txt").Return(errors.New("connection refused"))

	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().EnqueueCleanup(gomock.Any(), "b1", "real.txt", "overwrite_displaced", int64(256)).
		Return(nil).Times(1)
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), "b1", int64(256)).Return(nil).Times(1)

	coord := newCoordinatorWithBackend("b1", be, store)
	coord.DeleteOrEnqueue(context.Background(), be, "b1", "real.txt", "overwrite_displaced", 256)
}

// TestRecoverFromRecordFailure_DeleteReturns404_SkipsEnqueue covers
// #880: the post-record-failure cleanup path must treat a backend 404
// the same way DeleteOrEnqueue does and skip the enqueue. Otherwise
// the cleanup queue accumulates phantom rows for objects the backend
// already agrees are gone.
func TestRecoverFromRecordFailure_DeleteReturns404_SkipsEnqueue(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	be := backendtest.NewMockObjectBackend(ctrl)
	be.EXPECT().DeleteObject(gomock.Any(), "phantom.txt").
		Return(&httpError{code: 404, msg: "NoSuchKey"})

	store := NewMockCoordinatorStores(ctrl)
	// The whole point of the fix: EnqueueCleanup must NOT be called.
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

	coord := newCoordinatorWithBackend("b1", be, store)
	coord.RecoverFromRecordFailure(context.Background(), be, "b1", "phantom.txt", "record_failure", 128)
}

// TestRecoverFromRecordFailure_GenericError_Enqueues pins the
// retain-the-existing-behavior contract: a non-404 cleanup failure
// still seeds the cleanup queue so the worker can retry.
func TestRecoverFromRecordFailure_GenericError_Enqueues(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	be := backendtest.NewMockObjectBackend(ctrl)
	be.EXPECT().DeleteObject(gomock.Any(), "real.txt").Return(errors.New("connection refused"))

	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().EnqueueCleanup(gomock.Any(), "b1", "real.txt", "record_failure", int64(256)).
		Return(nil).Times(1)
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), "b1", int64(256)).Return(nil).Times(1)

	coord := newCoordinatorWithBackend("b1", be, store)
	coord.RecoverFromRecordFailure(context.Background(), be, "b1", "real.txt", "record_failure", 256)
}

// TestRecordObjectAndPromoteIntent_UnknownBackend covers the legacy
// fallback path's "backend not registered" branch: with intentID empty
// and an unknown backend name, the method returns an error before any
// store call.
func TestRecordObjectAndPromoteIntent_UnknownBackend(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockCoordinatorStores(ctrl)

	coord := newCoordinatorWithStore(store, false)

	tracer := noop.NewTracerProvider().Tracer("test")
	_, sp := tracer.Start(context.Background(), "test")
	defer sp.End()

	err := coord.RecordObjectAndPromoteIntent(context.Background(), sp, &core.RecordObjectRequest{
		Key: "k", Backend: "no-such-backend", Size: 1024,
	})
	if err == nil {
		t.Fatal("expected error for unregistered backend")
	}
}
