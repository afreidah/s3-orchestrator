// -------------------------------------------------------------------------------
// Reconcile - Unit Tests
//
// Author: Alex Freidah
//
// Tests for the bounded-memory sorted-merge reconciliation. Three layers:
//
//   - Sorted: pure merge engine, exercised with slice-backed
//     iterators. Covers every merge branch (s3-only, db-only, equal,
//     interleaved, error propagation, empty inputs).
//   - dbCursorStream: paginated DB iterator. Covers cursor advancement,
//     end-of-stream, sibling-bucket filter, store-error propagation.
//   - helper functions: Unmanaged, ImportHandler,
//     DeleteHandler.
//
// The full end-to-end path (real *backend.S3Backend, real PostgreSQL) is
// covered by the integration suite; these tests run without external
// services.
// -------------------------------------------------------------------------------

package reconcile

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// Test helpers
// -------------------------------------------------------------------------

// sliceKeySource is a deterministic, slice-backed keySource that the merge
// engine consumes during unit tests.
type sliceKeySource struct {
	entries []Entry
	idx     int
	err     error // returned on the Nth call (zero-indexed)
	errAt   int
	stopped bool
}

// next satisfies the keySource interface for tests. Returns entries
// from the slice in order, optionally raising the configured error at
// the specified index so error-path branches in Sorted can be
// covered without a real backend or DB.
func (s *sliceKeySource) Next(_ context.Context) (Entry, bool, error) {
	if s.err != nil && s.idx == s.errAt {
		return Entry{}, false, s.err
	}
	if s.idx >= len(s.entries) {
		return Entry{}, false, nil
	}
	e := s.entries[s.idx]
	s.idx++
	return e, true, nil
}

// stop satisfies the keySource interface for tests. Records that stop
// was called so the test can assert Sorted releases its
// sources on every exit path (success, error, context cancellation).
func (s *sliceKeySource) Stop() { s.stopped = true }

// e is a tiny constructor for test entry literals.
func e(key string, size int64) Entry { return Entry{key: key, size: size} }

// runMerge invokes Sorted and collects the imports / deletes the
// engine emits. Helper because every merge test wants the same shape.
func runMerge(t *testing.T, s3, dbIter keySource) (imports []Entry, deletes []string, err error) {
	t.Helper()
	err = Sorted(context.Background(), s3, dbIter,
		func(_ context.Context, ent Entry) error {
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
// Sorted  -  every merge branch
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
	s3 := &sliceKeySource{entries: []Entry{e("a", 1), e("b", 2), e("c", 3)}}
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
	dbIter := &sliceKeySource{entries: []Entry{e("x", 0), e("y", 0)}}
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
// both sides -> no work.
func TestReconcileSorted_FullMatch(t *testing.T) {
	s3 := &sliceKeySource{entries: []Entry{e("a", 1), e("b", 2), e("c", 3)}}
	dbIter := &sliceKeySource{entries: []Entry{e("a", 0), e("b", 0), e("c", 0)}}
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
	s3 := &sliceKeySource{entries: []Entry{
		e("a", 1), e("c", 3), e("d", 4), e("f", 6),
	}}
	dbIter := &sliceKeySource{entries: []Entry{
		e("b", 0), e("d", 0), e("e", 0),
	}}
	imp, del, err := runMerge(t, s3, dbIter)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	wantImp := []string{"a", "c", "f"}
	wantDel := []string{"b", "e"}
	if got := keysOf(imp); !slices.Equal(got, wantImp) {
		t.Errorf("imports = %v, want %v", got, wantImp)
	}
	if !slices.Equal(del, wantDel) {
		t.Errorf("deletes = %v, want %v", del, wantDel)
	}
}

// TestReconcileSorted_ImportPreservesSize verifies that the size payload
// from the S3 stream survives the merge into the import callback.
func TestReconcileSorted_ImportPreservesSize(t *testing.T) {
	s3 := &sliceKeySource{entries: []Entry{e("k", 4096)}}
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
		entries: []Entry{e("a", 1), e("b", 2)},
		err:     want,
		errAt:   1, // fail on second next() call
	}
	dbIter := &sliceKeySource{entries: []Entry{e("c", 0)}}
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
		entries: []Entry{e("a", 0), e("b", 0)},
		err:     want,
		errAt:   1,
	}
	_, _, err := runMerge(t, &sliceKeySource{entries: []Entry{e("z", 1)}}, dbIter)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}

// TestReconcileSorted_S3PrimeErrorAborts verifies the very first call to
// s3.next, before the loop starts, surfaces from the merge. Covers the
// priming-error early return distinct from the in-loop error paths.
func TestReconcileSorted_S3PrimeErrorAborts(t *testing.T) {
	want := errors.New("s3 prime")
	s3 := &sliceKeySource{err: want, errAt: 0} // fail on first next()
	_, _, err := runMerge(t, s3, &sliceKeySource{})
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}

// TestReconcileSorted_DBPrimeErrorAborts verifies the priming call to
// dbIter.next surfaces from the merge.
func TestReconcileSorted_DBPrimeErrorAborts(t *testing.T) {
	want := errors.New("db prime")
	dbIter := &sliceKeySource{err: want, errAt: 0}
	_, _, err := runMerge(t, &sliceKeySource{entries: []Entry{e("a", 1)}}, dbIter)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}

// TestReconcileSorted_DeleteHandlerErrorAborts ensures onDelete failures
// propagate (mirrors TestReconcileSorted_HandlerErrorAborts on the import
// side, but explicitly drives the delete branch).
func TestReconcileSorted_DeleteHandlerErrorAborts(t *testing.T) {
	want := errors.New("delete broke")
	dbIter := &sliceKeySource{entries: []Entry{e("x", 0), e("y", 0)}}
	called := 0
	err := Sorted(context.Background(), &sliceKeySource{}, dbIter,
		func(_ context.Context, _ Entry) error { return nil },
		func(_ context.Context, _ string) error {
			called++
			return want
		},
	)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
	if called != 1 {
		t.Errorf("expected exactly one onDelete invocation before abort, got %d", called)
	}
}

// TestReconcileSorted_MatchStepS3AdvanceErrorAborts targets the S3-side
// advance inside matchStep: the first key matches, then the second
// s3.next call fails. Without this, matchStep's s3-advance error branch
// is never hit.
func TestReconcileSorted_MatchStepS3AdvanceErrorAborts(t *testing.T) {
	want := errors.New("s3 advance after match")
	s3 := &sliceKeySource{
		entries: []Entry{e("a", 1)},
		err:     want,
		errAt:   1, // fail when advanceS3 runs after the match
	}
	dbIter := &sliceKeySource{entries: []Entry{e("a", 0), e("b", 0)}}
	_, _, err := runMerge(t, s3, dbIter)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}

// TestReconcileSorted_HandlerErrorAborts verifies that an onImport /
// onDelete failure short-circuits the merge.
func TestReconcileSorted_HandlerErrorAborts(t *testing.T) {
	want := errors.New("import broke")
	s3 := &sliceKeySource{entries: []Entry{e("a", 1), e("b", 2), e("c", 3)}}
	called := 0
	err := Sorted(context.Background(), s3, &sliceKeySource{},
		func(_ context.Context, _ Entry) error {
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
// dbCursorStream  -  paginated DB iterator
// -------------------------------------------------------------------------

// fakeLister is a hand-rolled DBKeyLister that returns a slice of
// pre-batched pages. Each ListObjectsByBackendKeyAsc call pops one and
// applies the cursor + limit so tests can assert real pagination
// semantics rather than blindly returning whatever the test wired.
type fakeLister struct {
	pages [][]core.ObjectLocation
	calls int
	err   error
	errAt int
}

// ListObjectsByBackendKeyAsc lists objects by backend key asc.
func (f *fakeLister) ListObjectsByBackendKeyAsc(_ context.Context, _, afterKey string, limit int) ([]core.ObjectLocation, error) {
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
	out := make([]core.ObjectLocation, 0, len(page))
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
		pages: [][]core.ObjectLocation{
			{{ObjectKey: "vb/a"}, {ObjectKey: "vb/b"}},
			{{ObjectKey: "vb/c"}, {ObjectKey: "vb/d"}},
		},
	}
	it := NewDBCursorStream(DBCursorStreamDeps{Store: lister, BackendName: "be1"})
	got := drainStream(t, it)
	want := []string{"vb/a", "vb/b", "vb/c", "vb/d"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestDBCursorStream_YieldsEveryBucket verifies the cursor emits rows for every
// virtual bucket on the backend. Reconcile is backend-scoped, and a row the
// cursor skipped would look backend-only to the merge and be re-imported on
// every pass.
func TestDBCursorStream_YieldsEveryBucket(t *testing.T) {
	lister := &fakeLister{
		pages: [][]core.ObjectLocation{
			{
				{ObjectKey: "other/x"},
				{ObjectKey: "vb/a"},
				{ObjectKey: "vb/b"},
			},
		},
	}
	it := NewDBCursorStream(DBCursorStreamDeps{Store: lister, BackendName: "be1"})
	got := drainStream(t, it)
	if !slices.Equal(got, []string{"other/x", "vb/a", "vb/b"}) {
		t.Errorf("got %v, want every row for the backend", got)
	}
}

// TestDBCursorStream_PropagatesError verifies the cursor surfaces a
// store-side error. fakeLister.errAt=1 fails the second list call, so the
// iterator returns page 1 successfully then errs on the page-2 fetch.
func TestDBCursorStream_PropagatesError(t *testing.T) {
	want := errors.New("query failed")
	lister := &fakeLister{
		pages: [][]core.ObjectLocation{
			{{ObjectKey: "vb/a"}, {ObjectKey: "vb/b"}},
		},
		err:   want,
		errAt: 1, // fail on the second page fetch
	}
	it := NewDBCursorStream(DBCursorStreamDeps{Store: lister, BackendName: "be1"})
	ctx := context.Background()

	// Page 1  -  should succeed.
	for i, want := range []string{"vb/a", "vb/b"} {
		ent, ok, err := it.Next(ctx)
		if err != nil || !ok || ent.key != want {
			t.Fatalf("page-1[%d]: got (%v,%v,%v), want (%s,true,nil)", i, ent.key, ok, err, want)
		}
	}

	// Page 2 fetch  -  fakeLister returns the configured error.
	_, _, err := it.Next(ctx)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}

// TestDBCursorStream_StopIsNoop confirms stop is callable and idempotent
//   - it has no goroutine to halt, so the contract is "does not panic and
//
// has no side effect on subsequent next calls."
func TestDBCursorStream_StopIsNoop(t *testing.T) {
	t.Parallel()
	lister := &fakeLister{pages: [][]core.ObjectLocation{{{ObjectKey: "vb/a"}}}}
	it := NewDBCursorStream(DBCursorStreamDeps{Store: lister, BackendName: "be1"})
	it.Stop()
	it.Stop() // idempotent
	got := drainStream(t, it)
	if !slices.Equal(got, []string{"vb/a"}) {
		t.Errorf("stop should not affect iteration, got %v", got)
	}
}

// TestDBCursorStream_ContextCancellation verifies the cursor stops trying
// to fetch new pages once the context is cancelled.
func TestDBCursorStream_ContextCancellation(t *testing.T) {
	lister := &fakeLister{pages: [][]core.ObjectLocation{
		{{ObjectKey: "vb/a"}},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	it := NewDBCursorStream(DBCursorStreamDeps{Store: lister, BackendName: "be1"})
	_, ok, err := it.Next(ctx)
	if ok || err == nil {
		t.Errorf("cancelled ctx should abort: ok=%v err=%v", ok, err)
	}
}

// drain returns the keys produced by an iterator until exhausted, failing
// the test on any non-nil error.
// drainStream drain stream.
// drainStream drain stream.
func drainStream(t *testing.T, it keySource) []string {
	t.Helper()
	var out []string
	for {
		ent, ok, err := it.Next(context.Background())
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
// Unmanaged
// -------------------------------------------------------------------------

// TestUnmanaged covers the classification that replaces the old prefix
// rewriting: a key is managed when it sits under some configured virtual
// bucket, and unmanaged otherwise. Nothing is dropped and nothing is renamed.
func TestUnmanaged(t *testing.T) {
	prefixes := []string{"vb/", "other/"}
	cases := map[string]bool{
		"vb/foo":    false,
		"other/foo": false,
		"test.txt":  true,
		"foo/bar":   true,
		"vb":        true, // the bare bucket name is not under its own prefix
		"vbx/thing": true,
	}
	for key, want := range cases {
		if got := Unmanaged(key, prefixes); got != want {
			t.Errorf("Unmanaged(%q) = %v, want %v", key, got, want)
		}
	}
}

// TestUnmanaged_NoBuckets treats every key as unmanaged when nothing is
// configured, rather than silently adopting the whole backend.
func TestUnmanaged_NoBuckets(t *testing.T) {
	if !Unmanaged("anything", nil) {
		t.Error("with no configured buckets every key should be unmanaged")
	}
}

// -------------------------------------------------------------------------
// s3KeyStream  -  goroutine-backed iterator over the S3 page callback
// -------------------------------------------------------------------------

// fakeLister implements ObjectLister by feeding the supplied pages into
// the callback and optionally returning an error after the last page.
type fakeLister2 struct {
	pages [][]backend.ListedObject
	err   error
}

// ListObjects lists objects.
func (f *fakeLister2) ListObjects(_ context.Context, _ string, fn func([]backend.ListedObject) error) error {
	for _, p := range f.pages {
		if err := fn(p); err != nil {
			return err
		}
	}
	return f.err
}

// TestS3KeyStream_EmitsEveryKeyUntouched drives a multi-page walk and pins the
// property the merge depends on: keys come out exactly as the backend listed
// them, in the same order, with nothing dropped or rewritten. Rewriting only
// some keys is what broke the merge's ordering invariant.
func TestS3KeyStream_EmitsEveryKeyUntouched(t *testing.T) {
	pages := [][]backend.ListedObject{
		{{Key: "other/x", SizeBytes: 9}, {Key: "test.txt", SizeBytes: 2}},
		{{Key: "vb/a", SizeBytes: 1}, {Key: "vb/z", SizeBytes: 3}},
	}
	var apiPages atomic.Int64
	s3 := NewS3KeyStream(context.Background(),
		&fakeLister2{pages: pages}, []string{"vb/", "other/"}, &apiPages)
	defer s3.Stop()

	got := drainStream(t, s3)
	want := []string{"other/x", "test.txt", "vb/a", "vb/z"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := apiPages.Load(); got != 2 {
		t.Errorf("apiPages = %d, want 2", got)
	}
}

// TestS3KeyStream_TagsUnmanagedKeys confirms the stream carries the
// classification the importer needs, rather than encoding it in the key.
func TestS3KeyStream_TagsUnmanagedKeys(t *testing.T) {
	pages := [][]backend.ListedObject{{{Key: "test.txt"}, {Key: "vb/a"}}}
	s3 := NewS3KeyStream(context.Background(), &fakeLister2{pages: pages}, []string{"vb/"}, nil)
	defer s3.Stop()

	stray, _, _ := s3.Next(context.Background())
	if stray.key != "test.txt" || !stray.unmanaged {
		t.Errorf("stray = %+v, want test.txt tagged unmanaged", stray)
	}
	owned, _, _ := s3.Next(context.Background())
	if owned.key != "vb/a" || owned.unmanaged {
		t.Errorf("owned = %+v, want vb/a tagged managed", owned)
	}
}

// TestS3KeyStream_PropagatesListerError confirms that an error returned
// from ListObjects after the page callback drains surfaces from next.
func TestS3KeyStream_PropagatesListerError(t *testing.T) {
	want := errors.New("list boom")
	s3 := NewS3KeyStream(context.Background(),
		&fakeLister2{
			pages: [][]backend.ListedObject{{{Key: "vb/a", SizeBytes: 1}}},
			err:   want,
		}, []string{"vb/"}, nil)
	defer s3.Stop()

	// First entry comes through cleanly.
	if _, ok, err := s3.Next(context.Background()); err != nil || !ok {
		t.Fatalf("first next: ok=%v err=%v", ok, err)
	}
	// After channel closes, the deferred err is delivered on the next call.
	if _, _, err := s3.Next(context.Background()); !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}

// TestS3KeyStream_StopUnblocksGoroutine triggers stop while the producer
// goroutine is still trying to push entries; the test's success criterion
// is simply that next returns end-of-stream and stop does not deadlock.
func TestS3KeyStream_StopUnblocksGoroutine(t *testing.T) {
	pages := [][]backend.ListedObject{
		{{Key: "vb/a", SizeBytes: 1}, {Key: "vb/b", SizeBytes: 2}},
	}
	s3 := NewS3KeyStream(context.Background(),
		&fakeLister2{pages: pages}, []string{"vb/"}, nil)

	// Pull one entry, then stop without draining.
	if _, ok, err := s3.Next(context.Background()); err != nil || !ok {
		t.Fatalf("first next: ok=%v err=%v", ok, err)
	}
	s3.Stop()
	s3.Stop() // idempotent
}

// TestS3KeyStream_ContextCancelTerminates verifies that a context cancel
// while next is blocked propagates as an error.
func TestS3KeyStream_ContextCancelTerminates(t *testing.T) {
	// fakeLister2 with no pages  -  the channel will close cleanly, so we
	// exercise the ctx.Done branch by using a no-op lister that never
	// publishes anything and a cancelled ctx.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s3 := NewS3KeyStream(ctx, &fakeLister2{
		pages: [][]backend.ListedObject{{{Key: "vb/a", SizeBytes: 1}}},
	}, []string{"vb/"}, nil)
	defer s3.Stop()

	if _, _, err := s3.Next(ctx); err == nil {
		t.Error("cancelled ctx should produce an error from next")
	}
}

// -------------------------------------------------------------------------
// ImportHandler / DeleteHandler
// -------------------------------------------------------------------------

// TestImportHandler_CountsCreatedNotSkipped verifies the handler only bumps
// the counter when ImportObject reports the row was created (true).
func TestImportHandler_CountsCreatedNotSkipped(t *testing.T) {
	res := &Result{}
	importer := func(_ context.Context, _, _ string, _ int64, _ bool) (bool, error) {
		return false, nil // already exists; should NOT be counted
	}
	h := ImportHandler(slog.Default(), "b1", importer, res)
	if err := h(context.Background(), e("vb/foo", 1)); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.Imported != 0 {
		t.Errorf("imported = %d, want 0 (skipped row)", res.Imported)
	}
}

// TestImportHandler_SwallowsErrorButContinues verifies an import failure
// is logged but does not abort the merge  -  the merge would otherwise stop
// on the first transient row failure.
func TestImportHandler_SwallowsErrorButContinues(t *testing.T) {
	res := &Result{}
	importer := func(_ context.Context, _, _ string, _ int64, _ bool) (bool, error) {
		return false, errors.New("transient")
	}
	h := ImportHandler(slog.Default(), "b1", importer, res)
	if err := h(context.Background(), e("vb/foo", 1)); err != nil {
		t.Fatalf("handler should swallow error, got %v", err)
	}
	if res.Imported != 0 {
		t.Errorf("imported = %d, want 0", res.Imported)
	}
}

// TestDeleteHandler_CountsAndContinues verifies the delete path bumps the
// counter on success and swallows transient errors without aborting.
func TestDeleteHandler_CountsAndContinues(t *testing.T) {
	res := &Result{}
	var calls int
	deleter := func(_ context.Context, _, _ string) error {
		calls++
		if calls == 1 {
			return errors.New("transient")
		}
		return nil
	}
	h := DeleteHandler(slog.Default(), "b1", deleter, res)

	// First call: error swallowed, counter not bumped.
	_ = h(context.Background(), "vb/x")
	if res.Removed != 0 {
		t.Errorf("removed = %d after error, want 0", res.Removed)
	}

	// Second call: success, counter bumps.
	_ = h(context.Background(), "vb/y")
	if res.Removed != 1 {
		t.Errorf("removed = %d after success, want 1", res.Removed)
	}
}

// -------------------------------------------------------------------------
// String-slice equality helpers
// -------------------------------------------------------------------------

// keysOf projects a []Entry into its keys slice so the test
// can compare against the expected ordering with slices.Equal without
// exposing the entry struct fields the comparison does not care about.
func keysOf(es []Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.key
	}
	return out
}

// -------------------------------------------------------------------------
// Ascending-order guard
// -------------------------------------------------------------------------

// TestSorted_RejectsDescendingS3Stream pins the guard that would have caught
// the prefix-rewrite bug on its first pass. A backend stream that goes
// backwards used to make the merge delete every key in between and re-import
// them next pass, which looks like ordinary churn in the counts.
func TestSorted_RejectsDescendingS3Stream(t *testing.T) {
	s3 := &sliceKeySource{entries: []Entry{e("unified/test.txt", 1), e("unified/a", 1)}}
	db := &sliceKeySource{}

	err := Sorted(context.Background(), s3, db, noopImport, noopDelete)
	if !errors.Is(err, ErrNotAscending) {
		t.Fatalf("err = %v, want ErrNotAscending", err)
	}
	if !strings.Contains(err.Error(), "backend") {
		t.Errorf("err = %v, want it to name the offending stream", err)
	}
}

// TestSorted_RejectsDescendingDBStream covers the same guard on the ledger
// side, which enforces the COLLATE "C" cursor ordering the merge assumes.
func TestSorted_RejectsDescendingDBStream(t *testing.T) {
	s3 := &sliceKeySource{}
	db := &sliceKeySource{entries: []Entry{e("b", 1), e("a", 1)}}

	err := Sorted(context.Background(), s3, db, noopImport, noopDelete)
	if !errors.Is(err, ErrNotAscending) {
		t.Fatalf("err = %v, want ErrNotAscending", err)
	}
	if !strings.Contains(err.Error(), "ledger") {
		t.Errorf("err = %v, want it to name the offending stream", err)
	}
}

// TestSorted_RejectsRepeatedKey treats a repeated key as non-ascending too: a
// duplicate would make the merge act on the same key twice.
func TestSorted_RejectsRepeatedKey(t *testing.T) {
	s3 := &sliceKeySource{entries: []Entry{e("a", 1), e("a", 1)}}
	err := Sorted(context.Background(), s3, &sliceKeySource{}, noopImport, noopDelete)
	if !errors.Is(err, ErrNotAscending) {
		t.Fatalf("err = %v, want ErrNotAscending", err)
	}
}

// noopImport and noopDelete let the order guard be exercised without asserting
// on the callbacks.
func noopImport(context.Context, Entry) error  { return nil }
func noopDelete(context.Context, string) error { return nil }
