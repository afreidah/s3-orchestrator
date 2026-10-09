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
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/mock/gomock"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// httpError is a writepath-package test helper that satisfies the
// HTTPStatusCode() interface backend.IsNotFound classifies against.
type httpError struct {
	code int
	msg  string
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

func (e *httpError) Error() string       { return e.msg }
func (e *httpError) HTTPStatusCode() int { return e.code }

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

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
		Quota:    counter.NewQuotaTracker([]string{name}),
	})
	return New(c, store)
}

// newCoordinatorWithStore builds a minimal Coordinator backed by the
// supplied store. Avoids the live-fleet fixture so coordinator branches
// can be exercised in isolation without dragging real backends into
// every test.
func newCoordinatorWithStore(store CoordinatorStores) *Coordinator {
	c := infra.New(&infra.Config{
		Backends: map[string]s3be.ObjectBackend{},
		Quota:    counter.NewQuotaTracker(nil),
	})
	return New(c, store)
}

// newCoordinatorWith2Backends builds a Coordinator that knows a source and a
// destination backend, so MoveObject's StreamCopy (GetObject src -> PutObject
// dest) and its cleanup paths can be driven through the gomock recorders.
func newCoordinatorWith2Backends(srcName string, src s3be.ObjectBackend, destName string, dest s3be.ObjectBackend, store CoordinatorStores) *Coordinator {
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{srcName, destName}), nil)
	c := infra.New(&infra.Config{
		Backends:       map[string]s3be.ObjectBackend{srcName: src, destName: dest},
		Order:          []string{srcName, destName},
		BackendTimeout: 5 * time.Second,
		Usage:          usage,
		Quota:          counter.NewQuotaTracker([]string{srcName, destName}),
	})
	return New(c, store)
}

// expectStreamCopyOK wires the happy StreamCopy: read 4 bytes from src, write
// them to dest.
func expectStreamCopyOK(src, dest *backendtest.MockObjectBackend) {
	src.EXPECT().GetObject(gomock.Any(), srcPath, "").
		Return(&s3be.GetObjectResult{Body: io.NopCloser(strings.NewReader("data")), Size: 4}, nil)
	dest.EXPECT().PutObject(gomock.Any(), destPath, gomock.Any(), int64(4), gomock.Any(), gomock.Any()).
		Return("etag", nil)
}

// The two paths one move addresses: where the source copy sits, and where the
// move writes its own. They differ because a move is a write with its own
// path, so each of the three cleanup paths deletes exactly its own bytes.
const (
	srcPath  = "k!src-copy"
	destPath = "k!this-move"
)

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

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
	dest.EXPECT().DeleteObject(gomock.Any(), destPath).Return(errors.New("delete failed"))

	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().MoveObjectLocation(gomock.Any(), moveTo("k", "src", destName, destPath)).
		Return(int64(0), errors.New("cas failed"))
	store.EXPECT().EnqueueCleanup(gomock.Any(),
		cleanupOf(destName, destPath, RebalanceMoveReasons.Orphan, 4)).Return(nil).Times(1)
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
	dest.EXPECT().DeleteObject(gomock.Any(), destPath).Return(errors.New("delete failed"))

	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().MoveObjectLocation(gomock.Any(), moveTo("k", "src", destName, destPath)).Return(int64(0), nil)
	store.EXPECT().EnqueueCleanup(gomock.Any(),
		cleanupOf(destName, destPath, RebalanceMoveReasons.StaleOrphan, 4)).Return(nil).Times(1)
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
	src.EXPECT().DeleteObject(gomock.Any(), srcPath).Return(errors.New("delete failed"))

	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().MoveObjectLocation(gomock.Any(), moveTo("k", "src", destName, destPath)).Return(int64(4), nil)
	store.EXPECT().EnqueueCleanup(gomock.Any(),
		cleanupOf("src", srcPath, RebalanceMoveReasons.SourceDelete, 4)).Return(nil).Times(1)
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
		Key:            "k",
		SizeBytes:      4,
		SrcBackend:     src,
		SrcName:        "src",
		DestBackend:    dest,
		DestName:       destName,
		SrcStorageKey:  srcPath,
		DestStorageKey: destPath,
		Reasons:        RebalanceMoveReasons,
	}
}

// destName is the backend the move writes to, named once so the expectations
// and the request cannot drift apart.
const destName = "dest"

// cleanupOf matches an enqueued cleanup by the fields each move test is about:
// the backend, the path being deleted, the reason and the size.
func cleanupOf(backendName, storageKey, reason string, size int64) gomock.Matcher {
	return gomock.Cond(func(c *core.CleanupRequest) bool {
		return c.BackendName == backendName && c.StorageKey == storageKey &&
			c.Reason == reason && c.SizeBytes == size
	})
}

// moveTo matches the metadata CAS a move performs, including the path it is
// repointing the row at.
func moveTo(key, from, to, storageKey string) gomock.Matcher {
	return gomock.Cond(func(m *core.MoveLocation) bool {
		return m.ObjectKey == key && m.FromBackend == from &&
			m.ToBackend == to && m.StorageKey == storageKey
	})
}

// TestNewPendingIntent_CopiesStoredForm drives the form != nil branch so the
// PendingObject carries the wrapped DEK, keyID, plaintext size, and content
// hash. The intent is the only record of how the bytes were written until the
// commit lands, so a field missing here is a crash-recovered object whose row
// describes something nothing can read.
func TestNewPendingIntent_CopiesStoredForm(t *testing.T) {
	t.Parallel()
	form := &core.StoredForm{
		Encrypted:     true,
		EncryptionKey: []byte("wrapped-dek-bytes"),
		KeyID:         "kid-1",
		PlaintextSize: 4096,
		ContentHash:   "deadbeef",
	}

	got := NewPendingIntent("k", 4096, form, nil)

	if got.IntentID == "" {
		t.Fatal("expected a minted intent ID")
	}
	if got.BackendName != "" {
		t.Errorf("backend = %q, want it left for the claim to decide", got.BackendName)
	}
	if !got.Encrypted || got.KeyID != "kid-1" || got.PlaintextSize != 4096 || got.ContentHash != "deadbeef" {
		t.Errorf("encryption metadata not copied onto PendingObject: %+v", got)
	}
	if string(got.EncryptionKey) != "wrapped-dek-bytes" {
		t.Errorf("EncryptionKey not copied: %q", got.EncryptionKey)
	}
}

// TestNewPendingIntent_CopiesCompression asserts an intent carries the stored
// form's compression fields, and that SizeBytes is the stored size quota is
// judged against, not the larger logical size.
func TestNewPendingIntent_CopiesCompression(t *testing.T) {
	t.Parallel()
	form := &core.StoredForm{
		ContentHash:              "deadbeef",
		CompressionAlgorithm:     "zstd",
		CompressionLevel:         "default",
		CompressionFormatVersion: 1,
		LogicalSize:              8192,
	}

	got := NewPendingIntent("k", 4096, form, nil)

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

// TestClaimWriteTarget_TriesTheNextCandidateWhenOneIsFull asserts a backend
// whose insert declines is skipped rather than fatal. Declining is how a full
// backend reports itself now, so it has to read as "try the next one" and not
// as an error.
func TestClaimWriteTarget_TriesTheNextCandidateWhenOneIsFull(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockCoordinatorStores(ctrl)

	var claimed []string
	store.EXPECT().InsertPendingIfFits(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, p *core.PendingObject) (bool, error) {
			claimed = append(claimed, p.BackendName)
			return p.BackendName == "b2", nil
		}).Times(2)

	coord := newCoordinatorWithStore(store)

	name, err := coord.ClaimWriteTarget(context.Background(),
		NewPendingIntent("k", 4096, nil, nil), []string{"b1", "b2"})
	if err != nil {
		t.Fatalf("ClaimWriteTarget: %v", err)
	}
	if name != "b2" {
		t.Errorf("claimed %q, want b2 after b1 declined", name)
	}
	if want := []string{"b1", "b2"}; !slices.Equal(claimed, want) {
		t.Errorf("tried %v, want %v in order", claimed, want)
	}
}

// TestClaimWriteTarget_NoCandidateFits asserts the caller is told there is no
// room rather than being handed a backend that refused, so the request ends as
// insufficient storage instead of a write against a full backend.
func TestClaimWriteTarget_NoCandidateFits(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().InsertPendingIfFits(gomock.Any(), gomock.Any()).
		Return(false, nil).Times(2)

	coord := newCoordinatorWithStore(store)

	if _, err := coord.ClaimWriteTarget(context.Background(),
		NewPendingIntent("k", 4096, nil, nil), []string{"b1", "b2"}); !errors.Is(err, core.ErrNoSpaceAvailable) {
		t.Errorf("err = %v, want ErrNoSpaceAvailable", err)
	}
}

// TestClaimWriteTarget_StoreError covers the insert failure branch: a database
// error is surfaced rather than read as a full backend, because treating an
// outage as "no room" would silently place the write elsewhere.
func TestClaimWriteTarget_StoreError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().InsertPendingIfFits(gomock.Any(), gomock.Any()).
		Return(false, errors.New("db down")).Times(1)

	coord := newCoordinatorWithStore(store)

	name, err := coord.ClaimWriteTarget(context.Background(),
		NewPendingIntent("k", 4096, nil, nil), []string{"b1", "b2"})
	if err == nil {
		t.Fatal("expected the insert failure to surface")
	}
	if errors.Is(err, core.ErrNoSpaceAvailable) {
		t.Error("a database error was reported as a full backend")
	}
	if name != "" {
		t.Errorf("claimed %q, want no backend on error", name)
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
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any()).Times(0)

	coord := newCoordinatorWithBackend("b1", be, store)
	coord.DeleteOrEnqueue(context.Background(), be, &core.CleanupRequest{
		BackendName: "b1", ObjectKey: "phantom.txt", StorageKey: "phantom.txt",
		Reason: "overwrite_displaced", SizeBytes: 128,
	})
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
	store.EXPECT().EnqueueCleanup(gomock.Any(),
		cleanupOf("b1", "real.txt", "overwrite_displaced", 256)).Return(nil).Times(1)
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), "b1", int64(256)).Return(nil).Times(1)

	coord := newCoordinatorWithBackend("b1", be, store)
	coord.DeleteOrEnqueue(context.Background(), be, &core.CleanupRequest{
		BackendName: "b1", ObjectKey: "real.txt", StorageKey: "real.txt",
		Reason: "overwrite_displaced", SizeBytes: 256,
	})
}

// TestDeleteAllOrEnqueue_GroupsAndSettlesPerKey verifies requests are deleted
// one batch per backend and each settled on its own: a failure is queued, a 404
// is not, and a request on an unknown backend is skipped.
func TestDeleteAllOrEnqueue_GroupsAndSettlesPerKey(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	b1, b2 := backendtest.NewInMemory(), backendtest.NewInMemory()
	b1.BatchDeleteEnabled = true
	b1.BatchDeleteKeyErrs = map[string]error{
		"b": errors.New("denied"),
		"c": &httpError{code: 404, msg: "NoSuchKey"},
	}
	for _, k := range []string{"a", "b", "c"} {
		b1.Objects[k] = backendtest.Object{}
	}
	b2.Objects["x"] = backendtest.Object{}

	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().EnqueueCleanup(gomock.Any(), cleanupOf("b1", "b", "overwrite_displaced", 2)).Return(nil)
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), "b1", int64(2)).Return(nil)

	coord := newCoordinatorWith2Backends("b1", b1, "b2", b2, store)
	req := func(be, key string, size int64) *core.CleanupRequest {
		return &core.CleanupRequest{BackendName: be, ObjectKey: key, StorageKey: key, Reason: "overwrite_displaced", SizeBytes: size}
	}
	coord.DeleteAllOrEnqueue(context.Background(), []*core.CleanupRequest{
		req("b1", "a", 1), req("b1", "b", 2), req("b1", "c", 3), req("b2", "x", 4), req("gone", "y", 5),
	})

	if b1.BatchDeleteCalls != 1 {
		t.Errorf("b1 batch calls = %d, want one batch for its three copies", b1.BatchDeleteCalls)
	}
	if b1.Has("a") || b2.Has("x") {
		t.Error("a deleted copy is still on its backend")
	}
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
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any()).Times(0)

	coord := newCoordinatorWithBackend("b1", be, store)
	coord.RecoverFromRecordFailure(context.Background(), be, &core.CleanupRequest{
		BackendName: "b1", ObjectKey: "phantom.txt", StorageKey: "phantom.txt",
		Reason: "record_failure", SizeBytes: 128,
	})
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
	store.EXPECT().EnqueueCleanup(gomock.Any(),
		cleanupOf("b1", "real.txt", "record_failure", 256)).Return(nil).Times(1)
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), "b1", int64(256)).Return(nil).Times(1)

	coord := newCoordinatorWithBackend("b1", be, store)
	coord.RecoverFromRecordFailure(context.Background(), be, &core.CleanupRequest{
		BackendName: "b1", ObjectKey: "real.txt", StorageKey: "real.txt",
		Reason: "record_failure", SizeBytes: 256,
	})
}

// TestRecordObjectAndPromoteIntent_CleansUpWhatTheCommitDisplaced verifies the
// two kinds of bytes a commit hands back are deleted under their own cleanup
// reason, and at their own paths: a copy the write replaced, and an intent it
// superseded. Each names where its bytes are, which is not the object's key and
// not what this write just stored.
func TestRecordObjectAndPromoteIntent_CleansUpWhatTheCommitDisplaced(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	old := backendtest.NewMockObjectBackend(ctrl)
	stale := backendtest.NewMockObjectBackend(ctrl)
	old.EXPECT().DeleteObject(gomock.Any(), "k!replaced").Return(nil)
	stale.EXPECT().DeleteObject(gomock.Any(), "k!superseded").Return(nil)

	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().RecordObject(gomock.Any(), gomock.Any()).Return([]core.DeletedCopy{
		{BackendName: "old", StorageKey: "k!replaced", SizeBytes: 50},
		{BackendName: "stale", StorageKey: "k!superseded", SizeBytes: 70, Reason: core.CleanupReasonSupersededIntent},
	}, nil, nil)

	coord := newCoordinatorWith2Backends("old", old, "stale", stale, store)

	tracer := noop.NewTracerProvider().Tracer("test")
	_, sp := tracer.Start(context.Background(), "test")
	defer sp.End()

	err := coord.RecordObjectAndPromoteIntent(context.Background(), sp, &core.RecordObjectRequest{
		Key: "k", Size: 100,
		Copies: []core.ObjectCopy{{Backend: "new", IntentID: "i-1", StorageKey: "k!new"}},
	})
	if err != nil {
		t.Fatalf("RecordObjectAndPromoteIntent: %v", err)
	}
}

// TestRecordObjectAndPromoteIntent_RejectsMultipleCopies verifies the helper
// refuses a request placing more than one copy. Its recovery path deletes from
// a single backend, so it cannot account for the rest.
func TestRecordObjectAndPromoteIntent_RejectsMultipleCopies(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockCoordinatorStores(ctrl)

	coord := newCoordinatorWithStore(store)

	tracer := noop.NewTracerProvider().Tracer("test")
	_, sp := tracer.Start(context.Background(), "test")
	defer sp.End()

	err := coord.RecordObjectAndPromoteIntent(context.Background(), sp, &core.RecordObjectRequest{
		Key: "k", Size: 100,
		Copies: []core.ObjectCopy{{Backend: "b1", IntentID: "i-1"}, {Backend: "b2", IntentID: "i-2"}},
	})
	if err == nil {
		t.Fatal("expected a multi-copy request to be refused")
	}
}

// TestRecordObjectAndPromoteIntent_UnknownBackend covers the legacy
// fallback path's "backend not registered" branch: with intentID empty
// and an unknown backend name, the method returns an error before any
// store call.
func TestRecordObjectAndPromoteIntent_UnknownBackend(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockCoordinatorStores(ctrl)

	coord := newCoordinatorWithStore(store)

	tracer := noop.NewTracerProvider().Tracer("test")
	_, sp := tracer.Start(context.Background(), "test")
	defer sp.End()

	err := coord.RecordObjectAndPromoteIntent(context.Background(), sp, &core.RecordObjectRequest{
		Key: "k", Size: 1024, Copies: []core.ObjectCopy{{Backend: "no-such-backend"}},
	})
	if err == nil {
		t.Fatal("expected error for unregistered backend")
	}
}

// testSpan returns a no-op span for calls that take one.
func testSpan(t *testing.T) trace.Span {
	t.Helper()
	_, sp := noop.NewTracerProvider().Tracer("test").Start(context.Background(), "test")
	t.Cleanup(func() { sp.End() })
	return sp
}

// TestClaimUploadTarget_SkipsADecliningBackend verifies an upload the first
// candidate's insert declines, as a draining backend does, is recorded on the
// next candidate instead.
func TestClaimUploadTarget_SkipsADecliningBackend(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockCoordinatorStores(ctrl)
	var tried []string
	store.EXPECT().CreateMultipartUpload(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, p *core.CreateMultipartUploadParams) (bool, error) {
			tried = append(tried, p.BackendName)
			return len(tried) == 2, nil
		}).Times(2)

	coord := newCoordinatorWith2Backends("a", backendtest.NewMockObjectBackend(ctrl), "b", backendtest.NewMockObjectBackend(ctrl), store)
	params := &core.CreateMultipartUploadParams{UploadID: "u-1", ObjectKey: "k"}
	name, err := coord.ClaimUploadTarget(context.Background(), testSpan(t), s3op.CreateMultipartUpload, params)
	if err != nil {
		t.Fatalf("ClaimUploadTarget: %v", err)
	}
	if name != "b" || params.BackendName != "b" {
		t.Errorf("claimed %q (params %q), want b after a declined", name, params.BackendName)
	}
	if want := []string{"a", "b"}; !slices.Equal(tried, want) {
		t.Errorf("tried %v, want %v in order", tried, want)
	}
}

// TestClaimUploadTarget_EveryBackendDeclines verifies the claim reports no space
// when no candidate accepts the upload.
func TestClaimUploadTarget_EveryBackendDeclines(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().CreateMultipartUpload(gomock.Any(), gomock.Any()).Return(false, nil).Times(2)

	coord := newCoordinatorWith2Backends("a", backendtest.NewMockObjectBackend(ctrl), "b", backendtest.NewMockObjectBackend(ctrl), store)
	_, err := coord.ClaimUploadTarget(context.Background(), testSpan(t), s3op.CreateMultipartUpload,
		&core.CreateMultipartUploadParams{UploadID: "u-1", ObjectKey: "k"})
	if err == nil {
		t.Fatal("claimed an upload target when every backend declined")
	}
}

// TestClaimUploadTarget_DatabaseErrorEndsTheWalk verifies a store error is
// returned at once rather than read as a decline and tried elsewhere.
func TestClaimUploadTarget_DatabaseErrorEndsTheWalk(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().CreateMultipartUpload(gomock.Any(), gomock.Any()).Return(false, errors.New("db down")).Times(1)

	coord := newCoordinatorWith2Backends("a", backendtest.NewMockObjectBackend(ctrl), "b", backendtest.NewMockObjectBackend(ctrl), store)
	if _, err := coord.ClaimUploadTarget(context.Background(), testSpan(t), s3op.CreateMultipartUpload,
		&core.CreateMultipartUploadParams{UploadID: "u-1", ObjectKey: "k"}); err == nil {
		t.Fatal("ClaimUploadTarget returned no error for a database failure")
	}
}
