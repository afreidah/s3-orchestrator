// -------------------------------------------------------------------------------
// Reconcile Manager Tests - Sync and Reconciliation Orchestration
//
// Author: Alex Freidah
//
// Covers what the manager adds on top of the merge engine: resolving a
// backend's lister through its decorators, accounting listing pages against
// the usage quota, importing at the literal key, and composing a stale-row
// delete with its cleanup-queue sweep.
//
// Drives the manager against its own 4-method Stores interface rather than the
// 79-method union, so each test states only the store calls it cares about.
// -------------------------------------------------------------------------------

package reconcile

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// TEST DOUBLES
// -------------------------------------------------------------------------

// pagedLister is a backend that serves a fixed set of ListObjects pages. It
// implements only what the reconcile path touches; every other ObjectBackend
// method panics, so an unexpected call fails loudly rather than silently
// returning a zero value.
type pagedLister struct {
	*backendtest.InMemory
	pages   [][]backend.ListedObject
	listErr error
	calls   int
}

// newPagedLister builds a lister that also holds plaintext bytes for every key
// it lists, so the import path's envelope inspection reads real data.
func newPagedLister(pages [][]backend.ListedObject) *pagedLister {
	be := backendtest.NewInMemory()
	for _, page := range pages {
		for _, o := range page {
			be.Objects[o.Key] = backendtest.Object{Data: []byte("plaintext body")}
		}
	}
	return &pagedLister{InMemory: be, pages: pages}
}

func (f *pagedLister) ListObjects(ctx context.Context, prefix string, fn func([]backend.ListedObject) error) error {
	f.calls++
	if f.listErr != nil {
		return f.listErr
	}
	for _, page := range f.pages {
		if err := fn(page); err != nil {
			return err
		}
	}
	return nil
}

// decoratedBackend mimics a decorator (circuit breaker, metrics) around a
// backend, so resolveLister has something to unwrap.
type decoratedBackend struct {
	backend.ObjectBackend
	inner backend.ObjectBackend
}

func (w *decoratedBackend) Unwrap() backend.ObjectBackend { return w.inner }

// listlessBackend is a backend that cannot list, for the unsupported-backend path.
type listlessBackend struct{ backend.ObjectBackend }

// newTestManager wires a Manager over the generated mocks.
func newTestManager(t *testing.T, ctrl *gomock.Controller) (*Manager, *MockStores, *MockBackendResolver, *MockUsageRecorder) {
	t.Helper()
	stores := NewMockStores(ctrl)
	backends := NewMockBackendResolver(ctrl)
	usage := NewMockUsageRecorder(ctrl)
	// Admitted by default so each test states only the accounting it cares
	// about; the refusal path has its own test.
	usage.EXPECT().Allow(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(true).AnyTimes()
	return NewManager(&Deps{Backends: backends, Stores: stores, Usage: usage}), stores, backends, usage
}

// importOf matches the import request for one discovered key. The write time
// is deliberately not compared: it comes from whatever the backend reported and
// varies per fixture, where these tests are about which keys get imported.
func importOf(key, backendName string, size int64, unmanaged bool) gomock.Matcher {
	return gomock.Cond(func(req *core.ImportObjectRequest) bool {
		return req.Key == key && req.Backend == backendName &&
			req.Size == size && req.Unmanaged == unmanaged
	})
}

// ledgerRows returns a ListObjectsByBackendKeyAsc stub yielding one page then
// exhaustion, which is how the DB cursor signals the end of the walk.
func ledgerRows(rows ...core.ObjectLocation) func(context.Context, string, string, int) ([]core.ObjectLocation, error) {
	return func(_ context.Context, _, afterKey string, _ int) ([]core.ObjectLocation, error) {
		if afterKey == "" {
			return rows, nil
		}
		return nil, nil
	}
}

// -------------------------------------------------------------------------
// DELETER
// -------------------------------------------------------------------------

// TestDeleter_SweepsCleanupQueue asserts a stale-row delete is followed by the
// cleanup-queue sweep for the same key. Without the sweep, queue rows for a
// key the backend no longer holds keep retrying a delete that 404s until they
// exhaust their attempts.
func TestDeleter_SweepsCleanupQueue(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m, stores, _, _ := newTestManager(t, ctrl)

	stores.EXPECT().DeleteObjectLocation(gomock.Any(), "bucket/k1", "b1").Return(nil).Times(1)
	stores.EXPECT().SweepStaleCleanupQueueRows(gomock.Any(), "bucket/k1", "b1").Return(int64(0), nil).Times(1)

	if err := m.deleter()(t.Context(), "bucket/k1", "b1"); err != nil {
		t.Fatalf("deleter: %v", err)
	}
}

// TestDeleter_SweepFailureNotPropagated asserts a failed sweep is logged and
// swallowed: the row is already gone, so the next pass can retry the sweep.
func TestDeleter_SweepFailureNotPropagated(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m, stores, _, _ := newTestManager(t, ctrl)

	stores.EXPECT().DeleteObjectLocation(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
	stores.EXPECT().SweepStaleCleanupQueueRows(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(int64(0), errors.New("sweep boom"))

	if err := m.deleter()(t.Context(), "bucket/k1", "b1"); err != nil {
		t.Errorf("sweep failure must not propagate, got %v", err)
	}
}

// TestDeleter_DeleteFailurePropagates asserts the row delete itself is not
// best-effort, and that a failed delete skips the sweep.
func TestDeleter_DeleteFailurePropagates(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m, stores, _, _ := newTestManager(t, ctrl)

	want := errors.New("delete boom")
	stores.EXPECT().DeleteObjectLocation(gomock.Any(), gomock.Any(), gomock.Any()).Return(want)
	stores.EXPECT().SweepStaleCleanupQueueRows(gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

	if err := m.deleter()(t.Context(), "bucket/k1", "b1"); !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}

// -------------------------------------------------------------------------
// BACKEND RESOLUTION
// -------------------------------------------------------------------------

// TestResolveLister_UnwrapsDecorators asserts the resolver digs past wrapping
// backends to reach the client that can actually list.
func TestResolveLister_UnwrapsDecorators(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m, _, backends, _ := newTestManager(t, ctrl)

	inner := &pagedLister{InMemory: backendtest.NewInMemory()}
	backends.EXPECT().GetBackend("b1").Return(&decoratedBackend{ObjectBackend: inner, inner: inner}, nil).AnyTimes()

	got, err := m.resolveLister("b1")
	if err != nil {
		t.Fatalf("resolveLister: %v", err)
	}
	if got != ObjectLister(inner) {
		t.Error("resolveLister did not unwrap to the listing-capable backend")
	}
}

// TestResolveLister_UnknownBackend surfaces the lookup error unchanged.
func TestResolveLister_UnknownBackend(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m, _, backends, _ := newTestManager(t, ctrl)

	want := errors.New("no such backend")
	backends.EXPECT().GetBackend(gomock.Any()).Return(nil, want).AnyTimes()

	if _, err := m.resolveLister("nope"); !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}

// TestResolveLister_NotAListerFails asserts a backend that cannot list is a
// clear error rather than a nil-interface panic later in the merge.
func TestResolveLister_NotAListerFails(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m, _, backends, _ := newTestManager(t, ctrl)

	backends.EXPECT().GetBackend(gomock.Any()).Return(&listlessBackend{}, nil).AnyTimes()

	_, err := m.resolveLister("b1")
	if err == nil {
		t.Fatal("expected an error for a backend that cannot list")
	}
}

// -------------------------------------------------------------------------
// RECONCILE
// -------------------------------------------------------------------------

// TestReconcileBackend_ImportsAndDeletes drives the merge end to end: keys only
// on the backend are imported, keys only in the ledger are deleted, and keys on
// both sides are left alone.
func TestReconcileBackend_ImportsAndDeletes(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m, stores, backends, usage := newTestManager(t, ctrl)

	backends.EXPECT().GetBackend("b1").Return(newPagedLister([][]backend.ListedObject{{
		{Key: "vb/a", SizeBytes: 1}, {Key: "vb/b", SizeBytes: 2}, {Key: "vb/c", SizeBytes: 3},
	}}), nil).AnyTimes()
	stores.EXPECT().ListObjectsByBackendKeyAsc(gomock.Any(), "b1", gomock.Any(), gomock.Any()).
		DoAndReturn(ledgerRows(
			core.ObjectLocation{ObjectKey: "vb/b", BackendName: "b1"},
			core.ObjectLocation{ObjectKey: "vb/x", BackendName: "b1"},
		)).AnyTimes()

	// a and c are backend-only; b matches; x is ledger-only.
	stores.EXPECT().ImportObject(gomock.Any(), importOf("vb/a", "b1", 1, false)).Return(core.ImportInserted, nil)
	stores.EXPECT().ImportObject(gomock.Any(), importOf("vb/c", "b1", 3, false)).Return(core.ImportInserted, nil)
	stores.EXPECT().DeleteObjectLocation(gomock.Any(), "vb/x", "b1").Return(nil)
	stores.EXPECT().SweepStaleCleanupQueueRows(gomock.Any(), "vb/x", "b1").Return(int64(0), nil)
	usage.EXPECT().APICalls("b1", gomock.Any()).Times(1)

	res, err := m.ReconcileBackend(t.Context(), "b1", []string{"vb"})
	if err != nil {
		t.Fatalf("ReconcileBackend: %v", err)
	}
	if res.Imported != 2 || res.Removed != 1 {
		t.Errorf("res = %+v, want 2 imported / 1 removed", res)
	}
}

// TestReconcileBackend_NoMutationsWhenInSync covers the steady state, which is
// what every pass after the first should look like.
func TestReconcileBackend_NoMutationsWhenInSync(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m, stores, backends, usage := newTestManager(t, ctrl)

	backends.EXPECT().GetBackend("b1").Return(newPagedLister([][]backend.ListedObject{{{Key: "vb/a", SizeBytes: 1}, {Key: "vb/b", SizeBytes: 2}}}), nil).AnyTimes()
	stores.EXPECT().ListObjectsByBackendKeyAsc(gomock.Any(), "b1", gomock.Any(), gomock.Any()).
		DoAndReturn(ledgerRows(
			core.ObjectLocation{ObjectKey: "vb/a"},
			core.ObjectLocation{ObjectKey: "vb/b"},
		)).AnyTimes()
	usage.EXPECT().APICalls(gomock.Any(), gomock.Any()).AnyTimes()

	// Any mutation at all would be a bug.
	stores.EXPECT().ImportObject(gomock.Any(), gomock.Any()).Times(0)
	stores.EXPECT().DeleteObjectLocation(gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

	res, err := m.ReconcileBackend(t.Context(), "b1", []string{"vb"})
	if err != nil {
		t.Fatalf("ReconcileBackend: %v", err)
	}
	if res.Imported != 0 || res.Removed != 0 {
		t.Errorf("res = %+v, want no mutations", res)
	}
}

// TestReconcileBackend_StrayImportedAsUnmanaged covers a key outside every
// configured bucket prefix: it is imported at its literal key and flagged
// unmanaged, so it counts toward quota without any worker acting on it.
func TestReconcileBackend_StrayImportedAsUnmanaged(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m, stores, backends, usage := newTestManager(t, ctrl)

	backends.EXPECT().GetBackend("b1").Return(newPagedLister([][]backend.ListedObject{{{Key: "stray.txt", SizeBytes: 9}}}), nil).AnyTimes()
	stores.EXPECT().ListObjectsByBackendKeyAsc(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, nil).AnyTimes()
	usage.EXPECT().APICalls(gomock.Any(), gomock.Any()).AnyTimes()

	// unmanaged = true, and the key is untouched.
	stores.EXPECT().ImportObject(gomock.Any(), importOf("stray.txt", "b1", 9, true)).Return(core.ImportInserted, nil)

	res, err := m.ReconcileBackend(t.Context(), "b1", []string{"vb"})
	if err != nil {
		t.Fatalf("ReconcileBackend: %v", err)
	}
	if res.Imported != 1 {
		t.Errorf("Imported = %d, want 1", res.Imported)
	}
}

// TestReconcileBackend_StrayDoesNotChurn is the regression guard for the
// oscillation bug: a stray key already on the ledger must match rather than be
// deleted and re-imported on every pass.
func TestReconcileBackend_StrayDoesNotChurn(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m, stores, backends, usage := newTestManager(t, ctrl)

	backends.EXPECT().GetBackend("b1").Return(newPagedLister([][]backend.ListedObject{{{Key: "stray.txt", SizeBytes: 9}, {Key: "vb/a", SizeBytes: 1}}}), nil).AnyTimes()
	stores.EXPECT().ListObjectsByBackendKeyAsc(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(ledgerRows(
			core.ObjectLocation{ObjectKey: "stray.txt"},
			core.ObjectLocation{ObjectKey: "vb/a"},
		)).AnyTimes()
	usage.EXPECT().APICalls(gomock.Any(), gomock.Any()).AnyTimes()

	stores.EXPECT().ImportObject(gomock.Any(), gomock.Any()).Times(0)
	stores.EXPECT().DeleteObjectLocation(gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

	res, err := m.ReconcileBackend(t.Context(), "b1", []string{"vb"})
	if err != nil {
		t.Fatalf("ReconcileBackend: %v", err)
	}
	if res.Imported != 0 || res.Removed != 0 {
		t.Errorf("res = %+v, want a stray already on the ledger to be left alone", res)
	}
}

// TestReconcileBackend_PropagatesListingError surfaces a transport failure and
// still reports the partial tally rather than discarding it.
func TestReconcileBackend_PropagatesListingError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m, stores, backends, usage := newTestManager(t, ctrl)

	want := errors.New("list boom")
	backends.EXPECT().GetBackend(gomock.Any()).Return(&pagedLister{InMemory: backendtest.NewInMemory(), listErr: want}, nil).AnyTimes()
	stores.EXPECT().ListObjectsByBackendKeyAsc(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, nil).AnyTimes()
	usage.EXPECT().APICalls(gomock.Any(), gomock.Any()).AnyTimes()

	res, err := m.ReconcileBackend(t.Context(), "b1", []string{"vb"})
	if err == nil {
		t.Fatal("expected the listing error to propagate")
	}
	if res == nil {
		t.Error("a failed pass should still report its partial tally")
	}
}

// TestReconcileBackend_UnknownBackendFails covers the resolution failure path.
func TestReconcileBackend_UnknownBackendFails(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m, _, backends, _ := newTestManager(t, ctrl)

	backends.EXPECT().GetBackend(gomock.Any()).Return(nil, errors.New("unknown")).AnyTimes()

	if _, err := m.ReconcileBackend(t.Context(), "nope", nil); err == nil {
		t.Error("expected an error for an unknown backend")
	}
}

// TestReconcileBackend_CoversEveryVirtualBucket asserts one pass walks the
// whole backend rather than one bucket, which is what keeps both streams in
// the byte order the merge requires.
func TestReconcileBackend_CoversEveryVirtualBucket(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m, stores, backends, usage := newTestManager(t, ctrl)

	backends.EXPECT().GetBackend("b1").Return(newPagedLister([][]backend.ListedObject{{{Key: "one/a", SizeBytes: 1}, {Key: "two/b", SizeBytes: 2}}}), nil).AnyTimes()
	stores.EXPECT().ListObjectsByBackendKeyAsc(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, nil).AnyTimes()
	usage.EXPECT().APICalls(gomock.Any(), gomock.Any()).AnyTimes()

	// Both buckets are imported as managed in the same pass.
	stores.EXPECT().ImportObject(gomock.Any(), importOf("one/a", "b1", 1, false)).Return(core.ImportInserted, nil)
	stores.EXPECT().ImportObject(gomock.Any(), importOf("two/b", "b1", 2, false)).Return(core.ImportInserted, nil)

	res, err := m.ReconcileBackend(t.Context(), "b1", []string{"one", "two"})
	if err != nil {
		t.Fatalf("ReconcileBackend: %v", err)
	}
	if res.Imported != 2 {
		t.Errorf("Imported = %d, want 2 across both buckets", res.Imported)
	}
}

// -------------------------------------------------------------------------
// SYNC
// -------------------------------------------------------------------------

// TestSyncBackend_ImportsAndCountsSkips asserts sync separates rows it newly
// inserted from rows the ledger already had.
func TestSyncBackend_ImportsAndCountsSkips(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m, stores, backends, usage := newTestManager(t, ctrl)

	backends.EXPECT().GetBackend("b1").Return(newPagedLister([][]backend.ListedObject{{{Key: "vb/new", SizeBytes: 1}, {Key: "vb/known", SizeBytes: 2}}}), nil).AnyTimes()
	stores.EXPECT().ImportObject(gomock.Any(), importOf("vb/new", "b1", 1, false)).Return(core.ImportInserted, nil)
	stores.EXPECT().ImportObject(gomock.Any(), importOf("vb/known", "b1", 2, false)).Return(core.ImportSkippedExisting, nil)
	usage.EXPECT().APICalls("b1", int64(1)).Times(1)

	imported, skipped, err := m.SyncBackend(t.Context(), "b1", "vb", []string{"vb"})
	if err != nil {
		t.Fatalf("SyncBackend: %v", err)
	}
	if imported != 1 || skipped != 1 {
		t.Errorf("imported=%d skipped=%d, want 1/1", imported, skipped)
	}
}

// TestSyncBackend_SkipsKeyWithPendingDelete asserts a key the store refuses
// because its delete is still outstanding is counted as skipped rather than
// imported. Counting it as imported would report a resurrection as success.
func TestSyncBackend_SkipsKeyWithPendingDelete(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m, stores, backends, usage := newTestManager(t, ctrl)

	backends.EXPECT().GetBackend("b1").Return(newPagedLister([][]backend.ListedObject{{{Key: "vb/deleted", SizeBytes: 1}}}), nil).AnyTimes()
	stores.EXPECT().ImportObject(gomock.Any(), importOf("vb/deleted", "b1", 1, false)).
		Return(core.ImportSkippedPendingCleanup, nil)
	usage.EXPECT().APICalls("b1", int64(1)).Times(1)

	imported, skipped, err := m.SyncBackend(t.Context(), "b1", "vb", []string{"vb"})
	if err != nil {
		t.Fatalf("SyncBackend: %v", err)
	}
	if imported != 0 || skipped != 1 {
		t.Errorf("imported=%d skipped=%d, want 0/1", imported, skipped)
	}
}

// TestSyncBackend_ImportErrorAborts asserts a failed import stops the pass
// rather than silently importing a partial bucket.
func TestSyncBackend_ImportErrorAborts(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m, stores, backends, usage := newTestManager(t, ctrl)

	backends.EXPECT().GetBackend(gomock.Any()).Return(newPagedLister([][]backend.ListedObject{{{Key: "vb/a", SizeBytes: 1}}}), nil).AnyTimes()
	stores.EXPECT().ImportObject(gomock.Any(), gomock.Any()).
		Return(core.ImportSkippedExisting, errors.New("import boom"))
	usage.EXPECT().APICalls(gomock.Any(), gomock.Any()).AnyTimes()

	if _, _, err := m.SyncBackend(t.Context(), "b1", "vb", []string{"vb"}); err == nil {
		t.Error("expected the import error to propagate")
	}
}

// TestSyncBackend_AccountsListingPages asserts every listing page is charged
// against the backend's API quota, so a sync shows up in the same counters a
// client request would.
func TestSyncBackend_AccountsListingPages(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m, stores, backends, usage := newTestManager(t, ctrl)

	backends.EXPECT().GetBackend(gomock.Any()).Return(newPagedLister([][]backend.ListedObject{
		{{Key: "vb/a", SizeBytes: 1}},
		{{Key: "vb/b", SizeBytes: 2}},
		{{Key: "vb/c", SizeBytes: 3}},
	}), nil).AnyTimes()
	stores.EXPECT().ImportObject(gomock.Any(), gomock.Any()).
		Return(core.ImportInserted, nil).AnyTimes()
	// One charge per page, as each is consumed. A single lump charge of 3 is
	// what let a walk finish before anything noticed it had gone over.
	usage.EXPECT().APICalls("b1", int64(1)).Times(3)

	if _, _, err := m.SyncBackend(t.Context(), "b1", "vb", []string{"vb"}); err != nil {
		t.Fatalf("SyncBackend: %v", err)
	}
}

// TestSyncBackend_RefusedWhenOutOfAPIBudget asserts a sync will not start
// against a backend with no request headroom. A reconcile of a large bucket is
// thousands of list calls, so a pass that ran anyway would spend a monthly
// quota that client traffic is then refused on.
func TestSyncBackend_RefusedWhenOutOfAPIBudget(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	stores := NewMockStores(ctrl)
	backends := NewMockBackendResolver(ctrl)
	usage := NewMockUsageRecorder(ctrl)
	usage.EXPECT().Allow("b1", int64(1), int64(0), int64(0)).Return(false)
	m := NewManager(&Deps{Backends: backends, Stores: stores, Usage: usage})

	// The backend is never resolved and no page is ever charged: the refusal
	// has to land before any of that, or it has not saved anything.
	backends.EXPECT().GetBackend(gomock.Any()).Return(&pagedLister{InMemory: backendtest.NewInMemory()}, nil).AnyTimes()
	usage.EXPECT().APICalls(gomock.Any(), gomock.Any()).Times(0)

	_, _, err := m.SyncBackend(t.Context(), "b1", "vb", []string{"vb"})
	if !errors.Is(err, core.ErrUsageLimitExceeded) {
		t.Fatalf("SyncBackend error = %v, want core.ErrUsageLimitExceeded", err)
	}
}

// TestSyncBackend_NoPagesChargesNothing asserts an empty bucket does not
// record a zero-page API call.
func TestSyncBackend_NoPagesChargesNothing(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m, _, backends, usage := newTestManager(t, ctrl)

	backends.EXPECT().GetBackend(gomock.Any()).Return(&pagedLister{InMemory: backendtest.NewInMemory()}, nil).AnyTimes()
	usage.EXPECT().APICalls(gomock.Any(), gomock.Any()).Times(0)

	imported, skipped, err := m.SyncBackend(t.Context(), "b1", "vb", []string{"vb"})
	if err != nil {
		t.Fatalf("SyncBackend: %v", err)
	}
	if imported != 0 || skipped != 0 {
		t.Errorf("imported=%d skipped=%d, want 0/0", imported, skipped)
	}
}

// TestSyncBackend_UnknownBackendFails covers resolution failure.
func TestSyncBackend_UnknownBackendFails(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m, _, backends, _ := newTestManager(t, ctrl)

	backends.EXPECT().GetBackend(gomock.Any()).Return(nil, errors.New("unknown")).AnyTimes()

	if _, _, err := m.SyncBackend(t.Context(), "nope", "vb", nil); err == nil {
		t.Error("expected an error for an unknown backend")
	}
}

// TestBucketPrefixes covers the mapping from configured bucket names to the
// key prefixes their objects live under.
func TestBucketPrefixes(t *testing.T) {
	t.Parallel()
	got := BucketPrefixes([]string{"one", "two"})
	if len(got) != 2 {
		t.Fatalf("BucketPrefixes returned %d entries, want 2", len(got))
	}
	for i, p := range got {
		if p == "" {
			t.Errorf("prefix %d is empty", i)
		}
	}
	if len(BucketPrefixes(nil)) != 0 {
		t.Error("no buckets should yield no prefixes")
	}
}

// TestSyncBackend_StopsAtTheAPIBudget is the regression pin for a walk that ran
// thousands of list calls past an exhausted budget and only recorded them once
// it was done. The second page must not be listed at all.
func TestSyncBackend_StopsAtTheAPIBudget(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	stores := NewMockStores(ctrl)
	backends := NewMockBackendResolver(ctrl)
	usage := NewMockUsageRecorder(ctrl)
	m := NewManager(&Deps{Backends: backends, Stores: stores, Usage: usage})

	lister := newPagedLister([][]backend.ListedObject{
		{{Key: "vb/a", SizeBytes: 1}},
		{{Key: "vb/b", SizeBytes: 2}},
	})
	backends.EXPECT().GetBackend("b1").Return(lister, nil).AnyTimes()

	// Entry price, then the first page's charge reports no headroom left.
	gomock.InOrder(
		usage.EXPECT().Allow("b1", int64(1), int64(0), int64(0)).Return(true),
		usage.EXPECT().Allow("b1", int64(1), int64(0), int64(0)).Return(false),
	)
	// Charged once, for the page that was actually fetched.
	usage.EXPECT().APICalls("b1", int64(1)).Times(1)
	stores.EXPECT().ImportObject(gomock.Any(), importOf("vb/a", "b1", 1, false)).
		Return(core.ImportInserted, nil)

	imported, _, err := m.SyncBackend(t.Context(), "b1", "vb", []string{"vb"})
	if err != nil {
		t.Fatalf("a pass that stopped at the limit did the work it could afford; want no error, got %v", err)
	}
	if imported != 1 {
		t.Errorf("imported = %d, want the single page the budget allowed", imported)
	}
}

// TestSyncBackend_ChargesEachPageAsItGoes holds the accounting shape: one
// charge per page, as it is consumed, not one lump at the end.
func TestSyncBackend_ChargesEachPageAsItGoes(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m, stores, backends, usage := newTestManager(t, ctrl)

	lister := newPagedLister([][]backend.ListedObject{
		{{Key: "vb/a", SizeBytes: 1}},
		{{Key: "vb/b", SizeBytes: 2}},
		{{Key: "vb/c", SizeBytes: 3}},
	})
	backends.EXPECT().GetBackend("b1").Return(lister, nil).AnyTimes()
	stores.EXPECT().ImportObject(gomock.Any(), gomock.Cond(func(req *core.ImportObjectRequest) bool {
		return req.Backend == "b1" && !req.Unmanaged
	})).
		Return(core.ImportInserted, nil).Times(3)

	// Three pages, three single-page charges. A lump charge would arrive as
	// one call for 3, which is what let the overage go unnoticed until it had
	// already happened.
	usage.EXPECT().APICalls("b1", int64(1)).Times(3)

	if _, _, err := m.SyncBackend(t.Context(), "b1", "vb", []string{"vb"}); err != nil {
		t.Fatalf("SyncBackend: %v", err)
	}
}

// TestReconcileBackend_RefusesAnExhaustedBackend pins the entry check this path
// never had: a reconcile could previously start against a backend with nothing
// left and walk it in full.
func TestReconcileBackend_RefusesAnExhaustedBackend(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	stores := NewMockStores(ctrl)
	backends := NewMockBackendResolver(ctrl)
	usage := NewMockUsageRecorder(ctrl)
	m := NewManager(&Deps{Backends: backends, Stores: stores, Usage: usage})

	backends.EXPECT().GetBackend("b1").Return(newPagedLister(nil), nil).AnyTimes()
	usage.EXPECT().Allow("b1", int64(1), int64(0), int64(0)).Return(false)

	_, err := m.ReconcileBackend(t.Context(), "b1", []string{"vb"})
	if !errors.Is(err, core.ErrUsageLimitExceeded) {
		t.Errorf("err = %v, want ErrUsageLimitExceeded", err)
	}
}
