// -------------------------------------------------------------------------------
// Reconcile - Bounded-Memory Sorted-Merge Backend Reconciliation
//
// Author: Alex Freidah
//
// Diffs the live key set on a backend against the metadata store using a
// streaming sorted-merge. Both inputs are walked in lexicographic key order
// (S3 ListObjectsV2 is spec-mandated UTF-8 ordered, the DB cursor uses
// ORDER BY object_key ASC) so the merge runs in O(page_size) memory
// regardless of backend object count. Replaces the previous "materialise
// every key into a map" implementation that OOM'd at scale.
// -------------------------------------------------------------------------------

package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// reconcileEntry is the unit consumed by the merge: a key already namespaced
// to the current virtual bucket and its size on whichever side produced it.
type reconcileEntry struct {
	key  string
	size int64
}

// keySource is a forward, lex-ordered, bounded-memory iterator over keys
// (with size on the S3 side; DB-only keys do not carry size since the merge
// only needs it on import).
type keySource interface {
	// next returns the next entry. ok=false signals end-of-stream; err is
	// non-nil only on transport / DB failure (callers must abort).
	next(ctx context.Context) (reconcileEntry, bool, error)

	// stop releases any backing goroutine. Safe to call multiple times.
	stop()
}

// -------------------------------------------------------------------------
// SORTED-MERGE ENGINE
// -------------------------------------------------------------------------

// reconcileSorted walks two ascending key streams in lockstep, invoking
// onImport for keys present only on s3 and onDelete for keys present only
// in the DB. Keys present on both sides are no-ops. Memory is bounded by
// each iterator's internal buffer.
//
// The first failing onImport / onDelete bubbles up; iterator errors do
// the same.
func reconcileSorted(
	ctx context.Context,
	s3, dbIter keySource,
	onImport func(ctx context.Context, e reconcileEntry) error,
	onDelete func(ctx context.Context, key string) error,
) error {
	s := &mergeState{s3: s3, db: dbIter, onImport: onImport, onDelete: onDelete}
	if err := s.advanceS3(ctx); err != nil {
		return err
	}
	if err := s.advanceDB(ctx); err != nil {
		return err
	}
	for !s.done() {
		if err := s.step(ctx); err != nil {
			return err
		}
	}
	return nil
}

// mergeState holds the rolling cursor pair plus the callbacks the merge
// loop dispatches to. Pulling the four-variable cursor state into a struct
// is the only way to extract the per-branch bodies into methods without
// passing pointers to every variable on every call — and the extraction is
// what keeps each method below the cognitive-complexity threshold.
type mergeState struct {
	s3, db   keySource
	s3Cur    reconcileEntry
	dbCur    reconcileEntry
	s3OK     bool
	dbOK     bool
	onImport func(ctx context.Context, e reconcileEntry) error
	onDelete func(ctx context.Context, key string) error
}

// done reports whether both streams are exhausted.
func (s *mergeState) done() bool { return !s.s3OK && !s.dbOK }

// step advances exactly one merge round, picking the branch (import,
// delete, or match) based on which side currently holds the smaller key.
func (s *mergeState) step(ctx context.Context) error {
	switch {
	case !s.dbOK || (s.s3OK && s.s3Cur.key < s.dbCur.key):
		return s.importStep(ctx)
	case !s.s3OK || s.s3Cur.key > s.dbCur.key:
		return s.deleteStep(ctx)
	default:
		return s.matchStep(ctx)
	}
}

// importStep fires onImport for the current S3 entry then pulls the next
// one. Used when the DB cursor is exhausted or the S3 key sorts before
// the DB key.
func (s *mergeState) importStep(ctx context.Context) error {
	if err := s.onImport(ctx, s.s3Cur); err != nil {
		return err
	}
	return s.advanceS3(ctx)
}

// deleteStep fires onDelete for the current DB key then pulls the next DB
// row. Used when the S3 stream is exhausted or the DB key sorts before
// the S3 key.
func (s *mergeState) deleteStep(ctx context.Context) error {
	if err := s.onDelete(ctx, s.dbCur.key); err != nil {
		return err
	}
	return s.advanceDB(ctx)
}

// matchStep advances both cursors. Used when the keys match — the row is
// present on both sides and no callback fires.
func (s *mergeState) matchStep(ctx context.Context) error {
	if err := s.advanceS3(ctx); err != nil {
		return err
	}
	return s.advanceDB(ctx)
}

// advanceS3 pulls the next entry from the S3 stream into the cursor pair.
func (s *mergeState) advanceS3(ctx context.Context) error {
	cur, ok, err := s.s3.next(ctx)
	if err != nil {
		return err
	}
	s.s3Cur, s.s3OK = cur, ok
	return nil
}

// advanceDB pulls the next entry from the DB stream into the cursor pair.
func (s *mergeState) advanceDB(ctx context.Context) error {
	cur, ok, err := s.db.next(ctx)
	if err != nil {
		return err
	}
	s.dbCur, s.dbOK = cur, ok
	return nil
}

// -------------------------------------------------------------------------
// S3 STREAM ITERATOR
// -------------------------------------------------------------------------

// s3StreamSize bounds the channel between the ListObjects callback and the
// merge consumer. Memory: O(s3StreamSize * average key length) at most.
const s3StreamSize = 1000

// objectLister is the narrow surface of *backend.S3Backend that
// s3KeyStream depends on. Defining it here keeps the iterator decoupled
// from the concrete S3 client and lets tests substitute a fake.
type objectLister interface {
	ListObjects(ctx context.Context, prefix string, fn func([]backend.ListedObject) error) error
}

// s3KeyStream inverts the page-callback shape of objectLister.ListObjects
// into a forward iterator. A single goroutine drives the callback,
// dropping keys that belong to other virtual buckets and namespacing the
// rest under bucketPrefix. apiPages, when non-nil, is incremented per
// page so the caller can record API usage.
type s3KeyStream struct {
	ch        chan reconcileEntry
	errCh     chan error
	pending   error
	cancel    context.CancelFunc
	closeOnce bool
}

// newS3KeyStream starts the goroutine that walks the backend and returns a
// keySource. The caller must invoke stop when done so a partial walk does
// not leak goroutines.
func newS3KeyStream(
	ctx context.Context,
	s3b objectLister,
	bucketPrefix string,
	otherPrefixes []string,
	apiPages *int64,
) *s3KeyStream {
	streamCtx, cancel := context.WithCancel(ctx)
	s := &s3KeyStream{
		ch:     make(chan reconcileEntry, s3StreamSize),
		errCh:  make(chan error, 1),
		cancel: cancel,
	}

	go func() {
		defer close(s.ch)
		err := s3b.ListObjects(streamCtx, "", func(objects []backend.ListedObject) error {
			if apiPages != nil {
				atomic.AddInt64(apiPages, 1)
			}
			for i := range objects {
				obj := &objects[i]
				key, ok := namespaceKey(obj.Key, bucketPrefix, otherPrefixes)
				if !ok {
					continue
				}
				select {
				case s.ch <- reconcileEntry{key: key, size: obj.SizeBytes}:
				case <-streamCtx.Done():
					return streamCtx.Err()
				}
			}
			return nil
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			s.errCh <- err
		}
		close(s.errCh)
	}()

	return s
}

// namespaceKey applies the bucket-routing rules used by the original
// realKeys map. Returns (namespaced, true) when the key should be visited
// or ("", false) when it belongs to a sibling bucket.
func namespaceKey(rawKey, bucketPrefix string, otherPrefixes []string) (string, bool) {
	if strings.HasPrefix(rawKey, bucketPrefix) {
		return rawKey, true
	}
	for _, p := range otherPrefixes {
		if strings.HasPrefix(rawKey, p) {
			return "", false
		}
	}
	return bucketPrefix + rawKey, true
}

func (s *s3KeyStream) next(ctx context.Context) (reconcileEntry, bool, error) {
	if s.pending != nil {
		return reconcileEntry{}, false, s.pending
	}
	select {
	case e, ok := <-s.ch:
		if ok {
			return e, true, nil
		}
		// Drain a possibly-pending error from the goroutine.
		if err, has := <-s.errCh; has && err != nil {
			s.pending = err
			return reconcileEntry{}, false, err
		}
		return reconcileEntry{}, false, nil
	case <-ctx.Done():
		s.pending = ctx.Err()
		return reconcileEntry{}, false, ctx.Err()
	}
}

func (s *s3KeyStream) stop() {
	if !s.closeOnce {
		s.closeOnce = true
		s.cancel()
	}
}

// -------------------------------------------------------------------------
// DB CURSOR ITERATOR
// -------------------------------------------------------------------------

// dbCursorPageSize bounds how many DB rows are pulled per round-trip.
// Memory: O(dbCursorPageSize * row size).
const dbCursorPageSize = 1000

// dbKeyLister is the narrow contract the cursor needs from the store.
type dbKeyLister interface {
	ListObjectsByBackendKeyAsc(ctx context.Context, backendName, afterKey string, limit int) ([]core.ObjectLocation, error)
}

// dbCursorStream walks store.ListObjectsByBackendKeyAsc one bounded page at
// a time and filters rows to those belonging to bucketPrefix. Keys for
// sibling buckets stored on the same backend are skipped.
type dbCursorStream struct {
	store         dbKeyLister
	backendName   string
	bucketPrefix  string
	otherPrefixes []string

	page      []core.ObjectLocation
	idx       int
	cursor    string
	exhausted bool
}

// newDBCursorStream prepares the iterator without issuing any query yet —
// the first next call pulls the first page.
func newDBCursorStream(s dbKeyLister, backendName, bucketPrefix string, otherPrefixes []string) *dbCursorStream {
	return &dbCursorStream{
		store:         s,
		backendName:   backendName,
		bucketPrefix:  bucketPrefix,
		otherPrefixes: otherPrefixes,
	}
}

func (d *dbCursorStream) next(ctx context.Context) (reconcileEntry, bool, error) {
	for {
		// Drain the in-memory page first.
		for d.idx < len(d.page) {
			row := d.page[d.idx]
			d.idx++
			d.cursor = row.ObjectKey
			if d.belongs(row.ObjectKey) {
				return reconcileEntry{key: row.ObjectKey, size: row.SizeBytes}, true, nil
			}
		}
		if d.exhausted {
			return reconcileEntry{}, false, nil
		}
		if err := ctx.Err(); err != nil {
			return reconcileEntry{}, false, err
		}
		rows, err := d.store.ListObjectsByBackendKeyAsc(ctx, d.backendName, d.cursor, dbCursorPageSize)
		if err != nil {
			return reconcileEntry{}, false, fmt.Errorf("page DB objects: %w", err)
		}
		if len(rows) == 0 {
			d.exhausted = true
			return reconcileEntry{}, false, nil
		}
		d.page = rows
		d.idx = 0
	}
}

// stop is a no-op for the DB cursor — the iterator owns no goroutine and
// holds no other resource that needs explicit teardown. Defined so the
// type satisfies keySource alongside s3KeyStream, which does need cleanup.
func (d *dbCursorStream) stop() {
	// Intentionally empty: nothing to release. Required to satisfy the
	// keySource interface uniformly with s3KeyStream, whose stop tears
	// down a producer goroutine.
}

// belongs reports whether a DB-side key falls within the current bucket
// prefix. Keys owned by sibling buckets are skipped so a single-bucket
// reconcile does not delete other buckets' rows on the same backend.
func (d *dbCursorStream) belongs(key string) bool {
	if !strings.HasPrefix(key, d.bucketPrefix) {
		return false
	}
	for _, p := range d.otherPrefixes {
		if strings.HasPrefix(key, p) {
			return false
		}
	}
	return true
}

// -------------------------------------------------------------------------
// RECONCILE HANDLERS
// -------------------------------------------------------------------------

// importHandler returns the onImport callback used by the merge. Failures
// are logged but do not abort the reconcile pass — a single import
// failure should not stop the diff for thousands of other keys.
func importHandler(backendName string, importer importerFn, result *reconcileResult) func(context.Context, reconcileEntry) error {
	return func(ctx context.Context, e reconcileEntry) error {
		imported, err := importer(ctx, e.key, backendName, e.size)
		if err != nil {
			slog.WarnContext(ctx, "Reconcile: import failed", "key", e.key, "backend", backendName, "error", err)
			return nil
		}
		if imported {
			result.imported++
		}
		return nil
	}
}

// deleteHandler returns the onDelete callback used by the merge. Failures
// are logged but do not abort the pass.
func deleteHandler(backendName string, deleter deleterFn, result *reconcileResult) func(context.Context, string) error {
	return func(ctx context.Context, key string) error {
		if err := deleter(ctx, key, backendName); err != nil {
			slog.WarnContext(ctx, "Reconcile: failed to remove stale entry", "key", key, "backend", backendName, "error", err)
			return nil
		}
		slog.InfoContext(ctx, "Reconcile: removed stale entry", "key", key, "backend", backendName)
		result.removed++
		return nil
	}
}

// reconcileResult is the in-progress accumulator handed to the import /
// delete callbacks. Promoted to a struct so tests can assert on it
// without importing internal/worker.
type reconcileResult struct {
	imported int64
	removed  int64
}

type importerFn func(ctx context.Context, key, backendName string, size int64) (bool, error)
type deleterFn func(ctx context.Context, key, backendName string) error
