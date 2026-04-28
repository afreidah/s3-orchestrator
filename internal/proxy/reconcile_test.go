// -------------------------------------------------------------------------------
// Reconcile - Unit Tests
//
// Author: Alex Freidah
//
// Tests for the bounded-memory sorted-merge reconciliation. Three layers:
//
//   - reconcileSorted: pure merge engine, exercised with slice-backed
//     iterators. Covers every merge branch (s3-only, db-only, equal,
//     interleaved, error propagation, empty inputs).
//   - dbCursorStream: paginated DB iterator. Covers cursor advancement,
//     end-of-stream, sibling-bucket filter, store-error propagation.
//   - helper functions: namespaceKey, siblingPrefixes, importHandler,
//     deleteHandler.
//
// The full end-to-end path (real *backend.S3Backend, real PostgreSQL) is
// covered by the integration suite; these tests run without external
// services.
// -------------------------------------------------------------------------------

package proxy

import (
	"context"
	"errors"
	"testing"

	st "github.com/afreidah/s3-orchestrator/internal/store"
)

// -------------------------------------------------------------------------
// Test helpers
// -------------------------------------------------------------------------

// sliceKeySource is a deterministic, slice-backed keySource that the merge
// engine consumes during unit tests.
type sliceKeySource struct {
	entries []reconcileEntry
	idx     int
	err     error // returned on the Nth call (zero-indexed)
	errAt   int
	stopped bool
}

func (s *sliceKeySource) next(_ context.Context) (reconcileEntry, bool, error) {
	if s.err != nil && s.idx == s.errAt {
		return reconcileEntry{}, false, s.err
	}
	if s.idx >= len(s.entries) {
		return reconcileEntry{}, false, nil
	}
	e := s.entries[s.idx]
	s.idx++
	return e, true, nil
}

func (s *sliceKeySource) stop() { s.stopped = true }

// e is a tiny constructor for test entry literals.
func e(key string, size int64) reconcileEntry { return reconcileEntry{key: key, size: size} }

// runMerge invokes reconcileSorted and collects the imports / deletes the
// engine emits. Helper because every merge test wants the same shape.
func runMerge(t *testing.T, s3, dbIter keySource) (imports []reconcileEntry, deletes []string, err error) {
	t.Helper()
	err = reconcileSorted(context.Background(), s3, dbIter,
		func(_ context.Context, ent reconcileEntry) error {
			imports = append(imports, ent)
			return nil
		},
		func(_ context.Context, key string) error {
			deletes = append(deletes, key)
			return nil
		},
	)
	return
}

// -------------------------------------------------------------------------
// reconcileSorted — every merge branch
// -------------------------------------------------------------------------

// TestReconcileSorted_EmptyBothInputs verifies the trivial case: no
// imports and no deletes when neither side has rows.
func TestReconcileSorted_EmptyBothInputs(t *testing.T) {
	imp, del, err := runMerge(t, &sliceKeySource{}, &sliceKeySource{})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if len(imp) != 0 || len(del) != 0 {
		t.Errorf("imports=%v deletes=%v, expected both empty", imp, del)
	}
}

// TestReconcileSorted_OnlyS3 covers the "DB exhausted, drain S3 as
// imports" branch.
func TestReconcileSorted_OnlyS3(t *testing.T) {
	s3 := &sliceKeySource{entries: []reconcileEntry{e("a", 1), e("b", 2), e("c", 3)}}
	imp, del, err := runMerge(t, s3, &sliceKeySource{})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if len(imp) != 3 || imp[0].key != "a" || imp[2].key != "c" {
		t.Errorf("imports = %v", imp)
	}
	if len(del) != 0 {
		t.Errorf("deletes should be empty, got %v", del)
	}
}

// TestReconcileSorted_OnlyDB covers the "S3 exhausted, drain DB as
// deletes" branch.
func TestReconcileSorted_OnlyDB(t *testing.T) {
	dbIter := &sliceKeySource{entries: []reconcileEntry{e("x", 0), e("y", 0)}}
	imp, del, err := runMerge(t, &sliceKeySource{}, dbIter)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if len(imp) != 0 {
		t.Errorf("imports should be empty, got %v", imp)
	}
	if len(del) != 2 || del[0] != "x" || del[1] != "y" {
		t.Errorf("deletes = %v", del)
	}
}

// TestReconcileSorted_FullMatch covers the equal-key path: every key on
// both sides → no work.
func TestReconcileSorted_FullMatch(t *testing.T) {
	s3 := &sliceKeySource{entries: []reconcileEntry{e("a", 1), e("b", 2), e("c", 3)}}
	dbIter := &sliceKeySource{entries: []reconcileEntry{e("a", 0), e("b", 0), e("c", 0)}}
	imp, del, err := runMerge(t, s3, dbIter)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if len(imp)+len(del) != 0 {
		t.Errorf("matched keys should produce no imports/deletes, got imp=%v del=%v", imp, del)
	}
}

// TestReconcileSorted_Interleaved covers a realistic mixed case: some S3
// keys missing from DB, some DB keys missing from S3, some shared.
func TestReconcileSorted_Interleaved(t *testing.T) {
	// S3:   a    c d   f
	// DB:     b    d e
	// Want: import {a, c, f}, delete {b, e}
	s3 := &sliceKeySource{entries: []reconcileEntry{
		e("a", 1), e("c", 3), e("d", 4), e("f", 6),
	}}
	dbIter := &sliceKeySource{entries: []reconcileEntry{
		e("b", 0), e("d", 0), e("e", 0),
	}}
	imp, del, err := runMerge(t, s3, dbIter)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	wantImp := []string{"a", "c", "f"}
	wantDel := []string{"b", "e"}
	if got := keysOf(imp); !equalStrings(got, wantImp) {
		t.Errorf("imports = %v, want %v", got, wantImp)
	}
	if !equalStrings(del, wantDel) {
		t.Errorf("deletes = %v, want %v", del, wantDel)
	}
}

// TestReconcileSorted_ImportPreservesSize verifies that the size payload
// from the S3 stream survives the merge into the import callback.
func TestReconcileSorted_ImportPreservesSize(t *testing.T) {
	s3 := &sliceKeySource{entries: []reconcileEntry{e("k", 4096)}}
	imp, _, err := runMerge(t, s3, &sliceKeySource{})
	if err != nil || len(imp) != 1 || imp[0].size != 4096 {
		t.Errorf("import size lost: %v err=%v", imp, err)
	}
}

// TestReconcileSorted_S3IteratorErrorAborts verifies that an S3-side
// transport failure surfaces from the merge.
func TestReconcileSorted_S3IteratorErrorAborts(t *testing.T) {
	want := errors.New("s3 boom")
	s3 := &sliceKeySource{
		entries: []reconcileEntry{e("a", 1), e("b", 2)},
		err:     want,
		errAt:   1, // fail on second next() call
	}
	dbIter := &sliceKeySource{entries: []reconcileEntry{e("c", 0)}}
	_, _, err := runMerge(t, s3, dbIter)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}

// TestReconcileSorted_DBIteratorErrorAborts verifies that a DB-side
// failure also surfaces from the merge.
func TestReconcileSorted_DBIteratorErrorAborts(t *testing.T) {
	want := errors.New("db boom")
	dbIter := &sliceKeySource{
		entries: []reconcileEntry{e("a", 0), e("b", 0)},
		err:     want,
		errAt:   1,
	}
	_, _, err := runMerge(t, &sliceKeySource{entries: []reconcileEntry{e("z", 1)}}, dbIter)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}

// TestReconcileSorted_HandlerErrorAborts verifies that an onImport /
// onDelete failure short-circuits the merge.
func TestReconcileSorted_HandlerErrorAborts(t *testing.T) {
	want := errors.New("import broke")
	s3 := &sliceKeySource{entries: []reconcileEntry{e("a", 1), e("b", 2), e("c", 3)}}
	called := 0
	err := reconcileSorted(context.Background(), s3, &sliceKeySource{},
		func(_ context.Context, _ reconcileEntry) error {
			called++
			return want
		},
		func(_ context.Context, _ string) error { return nil },
	)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
	if called != 1 {
		t.Errorf("expected handler called exactly once before abort, got %d", called)
	}
}

// -------------------------------------------------------------------------
// dbCursorStream — paginated DB iterator
// -------------------------------------------------------------------------

// fakeLister is a hand-rolled dbKeyLister that returns a slice of
// pre-batched pages. Each ListObjectsByBackendKeyAsc call pops one and
// applies the cursor + limit so tests can assert real pagination
// semantics rather than blindly returning whatever the test wired.
type fakeLister struct {
	pages [][]st.ObjectLocation
	calls int
	err   error
	errAt int
}

func (f *fakeLister) ListObjectsByBackendKeyAsc(_ context.Context, _, afterKey string, limit int) ([]st.ObjectLocation, error) {
	if f.err != nil && f.calls == f.errAt {
		f.calls++
		return nil, f.err
	}
	f.calls++
	if len(f.pages) == 0 {
		return nil, nil
	}
	page := f.pages[0]
	f.pages = f.pages[1:]
	out := make([]st.ObjectLocation, 0, len(page))
	for i := range page {
		if page[i].ObjectKey > afterKey {
			out = append(out, page[i])
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// TestDBCursorStream_DrainsAcrossPages verifies the iterator pulls page
// after page until ListObjectsByBackendKeyAsc returns empty.
func TestDBCursorStream_DrainsAcrossPages(t *testing.T) {
	lister := &fakeLister{
		pages: [][]st.ObjectLocation{
			{{ObjectKey: "vb/a"}, {ObjectKey: "vb/b"}},
			{{ObjectKey: "vb/c"}, {ObjectKey: "vb/d"}},
		},
	}
	it := newDBCursorStream(context.Background(), lister, "be1", "vb/", nil)
	got := drain(t, it)
	want := []string{"vb/a", "vb/b", "vb/c", "vb/d"}
	if !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestDBCursorStream_FiltersSiblingBucket verifies sibling-bucket rows are
// dropped — they belong to another bucket's reconcile pass.
func TestDBCursorStream_FiltersSiblingBucket(t *testing.T) {
	lister := &fakeLister{
		pages: [][]st.ObjectLocation{
			{
				{ObjectKey: "vb/a"},
				{ObjectKey: "other/x"}, // sibling bucket; must be skipped
				{ObjectKey: "vb/b"},
			},
		},
	}
	it := newDBCursorStream(context.Background(), lister, "be1", "vb/", []string{"other/"})
	got := drain(t, it)
	if !equalStrings(got, []string{"vb/a", "vb/b"}) {
		t.Errorf("sibling-bucket row not filtered: %v", got)
	}
}

// TestDBCursorStream_PropagatesError verifies the cursor surfaces a
// store-side error. fakeLister.errAt=1 fails the second list call, so the
// iterator returns page 1 successfully then errs on the page-2 fetch.
func TestDBCursorStream_PropagatesError(t *testing.T) {
	want := errors.New("query failed")
	lister := &fakeLister{
		pages: [][]st.ObjectLocation{
			{{ObjectKey: "vb/a"}, {ObjectKey: "vb/b"}},
		},
		err:   want,
		errAt: 1, // fail on the second page fetch
	}
	it := newDBCursorStream(context.Background(), lister, "be1", "vb/", nil)
	ctx := context.Background()

	// Page 1 — should succeed.
	for i, want := range []string{"vb/a", "vb/b"} {
		ent, ok, err := it.next(ctx)
		if err != nil || !ok || ent.key != want {
			t.Fatalf("page-1[%d]: got (%v,%v,%v), want (%s,true,nil)", i, ent.key, ok, err, want)
		}
	}

	// Page 2 fetch — fakeLister returns the configured error.
	_, _, err := it.next(ctx)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}

// TestDBCursorStream_ContextCancellation verifies the cursor stops trying
// to fetch new pages once the context is cancelled.
func TestDBCursorStream_ContextCancellation(t *testing.T) {
	lister := &fakeLister{pages: [][]st.ObjectLocation{
		{{ObjectKey: "vb/a"}},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	it := newDBCursorStream(ctx, lister, "be1", "vb/", nil)
	_, ok, err := it.next(ctx)
	if ok || err == nil {
		t.Errorf("cancelled ctx should abort: ok=%v err=%v", ok, err)
	}
}

// drain returns the keys produced by an iterator until exhausted, failing
// the test on any non-nil error.
func drain(t *testing.T, it keySource) []string {
	t.Helper()
	var out []string
	for {
		ent, ok, err := it.next(context.Background())
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
		if !ok {
			return out
		}
		out = append(out, ent.key)
	}
}

// -------------------------------------------------------------------------
// namespaceKey + siblingPrefixes
// -------------------------------------------------------------------------

// TestNamespaceKey_OwnBucket covers a key already namespaced to the
// current bucket — left untouched.
func TestNamespaceKey_OwnBucket(t *testing.T) {
	got, ok := namespaceKey("vb/foo", "vb/", []string{"other/"})
	if !ok || got != "vb/foo" {
		t.Errorf("got (%q,%v), want (vb/foo,true)", got, ok)
	}
}

// TestNamespaceKey_SiblingBucket covers a key that belongs to another
// configured virtual bucket — dropped.
func TestNamespaceKey_SiblingBucket(t *testing.T) {
	_, ok := namespaceKey("other/foo", "vb/", []string{"other/"})
	if ok {
		t.Error("sibling-bucket key should be dropped")
	}
}

// TestNamespaceKey_LegacyKey covers a key with no recognised prefix
// (legacy data not yet bucket-namespaced) — adopted under bucketPrefix.
func TestNamespaceKey_LegacyKey(t *testing.T) {
	got, ok := namespaceKey("foo/bar", "vb/", []string{"other/"})
	if !ok || got != "vb/foo/bar" {
		t.Errorf("got (%q,%v), want (vb/foo/bar,true)", got, ok)
	}
}

// TestSiblingPrefixes_DropsCurrent confirms the helper returns every other
// known bucket as a "/-suffixed prefix.
func TestSiblingPrefixes_DropsCurrent(t *testing.T) {
	got := siblingPrefixes([]string{"a", "b", "c"}, "b")
	want := []string{"a/", "c/"}
	if !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestSiblingPrefixes_NoMatch confirms a current-bucket value missing from
// the list does not corrupt the output.
func TestSiblingPrefixes_NoMatch(t *testing.T) {
	got := siblingPrefixes([]string{"a", "b"}, "z")
	want := []string{"a/", "b/"}
	if !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// -------------------------------------------------------------------------
// importHandler / deleteHandler
// -------------------------------------------------------------------------

// TestImportHandler_CountsCreatedNotSkipped verifies the handler only bumps
// the counter when ImportObject reports the row was created (true).
func TestImportHandler_CountsCreatedNotSkipped(t *testing.T) {
	res := &reconcileResult{}
	importer := func(_ context.Context, _, _ string, _ int64) (bool, error) {
		return false, nil // already exists; should NOT be counted
	}
	h := importHandler("b1", importer, res)
	if err := h(context.Background(), e("vb/foo", 1)); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.imported != 0 {
		t.Errorf("imported = %d, want 0 (skipped row)", res.imported)
	}
}

// TestImportHandler_SwallowsErrorButContinues verifies an import failure
// is logged but does not abort the merge — the merge would otherwise stop
// on the first transient row failure.
func TestImportHandler_SwallowsErrorButContinues(t *testing.T) {
	res := &reconcileResult{}
	importer := func(_ context.Context, _, _ string, _ int64) (bool, error) {
		return false, errors.New("transient")
	}
	h := importHandler("b1", importer, res)
	if err := h(context.Background(), e("vb/foo", 1)); err != nil {
		t.Fatalf("handler should swallow error, got %v", err)
	}
	if res.imported != 0 {
		t.Errorf("imported = %d, want 0", res.imported)
	}
}

// TestDeleteHandler_CountsAndContinues verifies the delete path bumps the
// counter on success and swallows transient errors without aborting.
func TestDeleteHandler_CountsAndContinues(t *testing.T) {
	res := &reconcileResult{}
	var calls int
	deleter := func(_ context.Context, _, _ string) error {
		calls++
		if calls == 1 {
			return errors.New("transient")
		}
		return nil
	}
	h := deleteHandler("b1", deleter, res)

	// First call: error swallowed, counter not bumped.
	_ = h(context.Background(), "vb/x")
	if res.removed != 0 {
		t.Errorf("removed = %d after error, want 0", res.removed)
	}

	// Second call: success, counter bumps.
	_ = h(context.Background(), "vb/y")
	if res.removed != 1 {
		t.Errorf("removed = %d after success, want 1", res.removed)
	}
}

// -------------------------------------------------------------------------
// String-slice equality helpers
// -------------------------------------------------------------------------

func keysOf(es []reconcileEntry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.key
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}