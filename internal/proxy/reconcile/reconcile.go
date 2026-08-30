// -------------------------------------------------------------------------------
// Reconcile - Bounded-Memory Sorted-Merge Backend Reconciliation
//
// Author: Alex Freidah
//
// Diffs the live key set on a backend against the metadata store using a
// streaming sorted-merge. Both inputs are walked in byte (C-collation) key
// order (S3 ListObjectsV2 is spec-mandated UTF-8 byte ordered; the DB cursor
// uses ORDER BY object_key COLLATE "C" ASC to match) so the merge runs in
// O(page_size) memory regardless of backend object count. The byte-order match
// is load-bearing: a locale-collated cursor mis-orders against the byte-order
// merge comparison and reconcile oscillates. Replaces the previous "materialise
// every key into a map" implementation that OOM'd at scale.
// -------------------------------------------------------------------------------

package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/internalkey"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// Entry is the unit consumed by the merge: a backend key exactly as it is
// stored, its size on whichever side produced it, and whether it falls inside
// a configured virtual bucket. Keys are never rewritten -- the merge compares
// them in byte order, and prepending a prefix to only some of them would break
// the ordering the whole design rests on.
type Entry struct {
	key       string
	size      int64
	unmanaged bool
}

// keySource is a forward, lex-ordered, bounded-memory iterator over keys
// (with size on the S3 side; DB-only keys do not carry size since the merge
// only needs it on import).
type keySource interface {
	// next returns the next entry. ok=false signals end-of-stream; err is
	// non-nil only on transport / DB failure (callers must abort).
	Next(ctx context.Context) (Entry, bool, error)

	// stop releases any backing goroutine. Safe to call multiple times.
	Stop()
}

// -------------------------------------------------------------------------
// SORTED-MERGE ENGINE
// -------------------------------------------------------------------------

// Sorted walks two ascending key streams in lockstep, invoking
// onImport for keys present only on s3 and onDelete for keys present only
// in the DB. Keys present on both sides are no-ops. Memory is bounded by
// each iterator's internal buffer.
//
// The first failing onImport / onDelete bubbles up; iterator errors do
// the same.
func Sorted(
	ctx context.Context,
	s3, dbIter keySource,
	onImport func(ctx context.Context, e Entry) error,
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

// mergeState holds the rolling cursor pair plus the callbacks the
// merge loop dispatches to. Bundling the four-variable cursor state
// lets per-branch advancement live in methods on mergeState rather
// than free functions taking pointers to every cursor.
type mergeState struct {
	s3, db   keySource
	s3Cur    Entry
	dbCur    Entry
	s3OK     bool
	dbOK     bool
	onImport func(ctx context.Context, e Entry) error
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

// matchStep advances both cursors. Used when the keys match  -  the row is
// present on both sides and no callback fires.
func (s *mergeState) matchStep(ctx context.Context) error {
	if err := s.advanceS3(ctx); err != nil {
		return err
	}
	return s.advanceDB(ctx)
}

// advanceS3 pulls the next entry from the S3 stream into the cursor pair.
func (s *mergeState) advanceS3(ctx context.Context) error {
	cur, ok, err := s.s3.Next(ctx)
	if err != nil {
		return err
	}
	if ok && s.s3OK {
		if err := checkAscending(sideBackend, s.s3Cur.key, cur.key); err != nil {
			return err
		}
	}
	s.s3Cur, s.s3OK = cur, ok
	return nil
}

// advanceDB pulls the next entry from the DB stream into the cursor pair.
func (s *mergeState) advanceDB(ctx context.Context) error {
	cur, ok, err := s.db.Next(ctx)
	if err != nil {
		return err
	}
	if ok && s.dbOK {
		if err := checkAscending(sideLedger, s.dbCur.key, cur.key); err != nil {
			return err
		}
	}
	s.dbCur, s.dbOK = cur, ok
	return nil
}

// The two sides of the merge, named in the ascending-order failure so an
// operator knows which stream to look at.
const (
	sideBackend = "backend listing"
	sideLedger  = "ledger cursor"
)

// ErrNotAscending reports a stream that handed the merge a key at or before its
// predecessor.
var ErrNotAscending = errors.New("reconcile stream is not in ascending key order")

// checkAscending enforces the invariant the whole merge rests on. A stream that
// goes backwards makes the merge delete every key between the two and re-import
// them on the next pass, which converges on nothing and looks like ordinary
// churn in the counts. Failing the pass turns that into one loud error naming
// the pair of keys that broke it.
func checkAscending(side, prev, next string) error {
	if next > prev {
		return nil
	}
	return fmt.Errorf("%w: the %s returned %q after %q", ErrNotAscending, side, next, prev)
}

// -------------------------------------------------------------------------
// S3 STREAM ITERATOR
// -------------------------------------------------------------------------

// s3StreamSize bounds the channel between the ListObjects callback and the
// merge consumer. Memory: O(s3StreamSize * average key length) at most.
const s3StreamSize = 1000

// ObjectLister is the narrow surface of *backend.S3Backend that
// S3KeyStream depends on. Defining it here keeps the iterator decoupled
// from the concrete S3 client and lets tests substitute a fake.
type ObjectLister interface {
	ListObjects(ctx context.Context, prefix string, fn func([]backend.ListedObject) error) error
}

// S3KeyStream inverts the page-callback shape of ObjectLister.ListObjects
// into a forward iterator. A single goroutine drives the callback, emitting
// every key the backend holds in the order it was listed and tagging each with
// whether it belongs to a configured virtual bucket. apiPages, when non-nil,
// is incremented per page so the caller can record API usage.
type S3KeyStream struct {
	ch        chan Entry
	errCh     chan error
	pending   error
	cancel    context.CancelFunc
	closeOnce bool
}

// NewS3KeyStream starts the goroutine that walks the backend and returns a
// keySource. The caller must invoke stop when done so a partial walk does
// not leak goroutines.
func NewS3KeyStream(
	ctx context.Context,
	s3b ObjectLister,
	bucketPrefixes []string,
	apiPages *atomic.Int64,
) *S3KeyStream {
	streamCtx, cancel := context.WithCancel(ctx)
	s := &S3KeyStream{
		ch:     make(chan Entry, s3StreamSize),
		errCh:  make(chan error, 1),
		cancel: cancel,
	}

	go s.run(streamCtx, s3b, bucketPrefixes, apiPages)

	return s
}

// run drives the backing ListObjects walk on the stream goroutine, forwarding
// each page through emitPage and surfacing any non-cancellation error on errCh.
func (s *S3KeyStream) run(ctx context.Context, s3b ObjectLister, bucketPrefixes []string, apiPages *atomic.Int64) {
	defer close(s.ch)
	err := s3b.ListObjects(ctx, "", func(objects []backend.ListedObject) error {
		return s.emitPage(ctx, objects, bucketPrefixes, apiPages)
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		s.errCh <- err
	}
	close(s.errCh)
}

// emitPage sends one ListObjects page onto the channel, counting the page and
// bailing out when the stream is cancelled. Keys pass through untouched, so the
// emitted sequence preserves the backend's byte ordering.
func (s *S3KeyStream) emitPage(ctx context.Context, objects []backend.ListedObject, bucketPrefixes []string, apiPages *atomic.Int64) error {
	if apiPages != nil {
		apiPages.Add(1)
	}
	for i := range objects {
		obj := &objects[i]
		select {
		case s.ch <- Entry{
			key:       obj.Key,
			size:      obj.SizeBytes,
			unmanaged: Unmanaged(obj.Key, bucketPrefixes),
		}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// BucketPrefixes converts configured virtual bucket names into the key
// prefixes their objects are stored under.
func BucketPrefixes(buckets []string) []string {
	out := make([]string, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, internalkey.Prefix(b))
	}
	return out
}

// Unmanaged reports whether a backend key falls outside every configured
// virtual bucket. Such a key is still reconciled and still counts toward the
// backend's quota, but no worker acts on it: the orchestrator did not put it
// there.
func Unmanaged(rawKey string, bucketPrefixes []string) bool {
	for _, p := range bucketPrefixes {
		if strings.HasPrefix(rawKey, p) {
			return false
		}
	}
	return true
}

// next pulls the next entry off the streaming channel that the
// background goroutine fills with backend-listed keys. Returns
// (entry, true, nil) on a successful read, (zero, false, nil) on
// graceful end-of-stream, or (zero, false, err) on either a producer
// error or a context cancellation. Once an error is observed it is
// latched into s.pending so subsequent calls see the same error
// instead of an empty channel.
func (s *S3KeyStream) Next(ctx context.Context) (Entry, bool, error) {
	if s.pending != nil {
		return Entry{}, false, s.pending
	}
	select {
	case e, ok := <-s.ch:
		if ok {
			return e, true, nil
		}
		// Drain a possibly-pending error from the goroutine.
		if err, has := <-s.errCh; has && err != nil {
			s.pending = err
			return Entry{}, false, err
		}
		return Entry{}, false, nil
	case <-ctx.Done():
		s.pending = ctx.Err()
		return Entry{}, false, ctx.Err()
	}
}

// stop cancels the producer goroutine if it is still running. Idempotent
// via closeOnce so multiple stop calls (Reconcile early-exits, deferred
// cleanup, error paths) do not double-close the cancel func.
func (s *S3KeyStream) Stop() {
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

// DBKeyLister is the narrow contract the cursor needs from the store.
type DBKeyLister interface {
	ListObjectsByBackendKeyAsc(ctx context.Context, backendName, afterKey string, limit int) ([]core.ObjectLocation, error)
}

// DBCursorStream walks store.ListObjectsByBackendKeyAsc one bounded page at a
// time, yielding every row recorded for the backend. Reconcile is scoped to a
// backend rather than to one virtual bucket, so nothing is filtered out here:
// a row the cursor skipped would look backend-only to the merge and be
// re-imported on every pass.
type DBCursorStream struct {
	store       DBKeyLister
	backendName string

	page      []core.ObjectLocation
	idx       int
	cursor    string
	exhausted bool
}

// DBCursorStreamDeps groups the cursor stream's parameters.
type DBCursorStreamDeps struct {
	Store       DBKeyLister
	BackendName string
}

// NewDBCursorStream prepares the iterator without issuing any query yet  -
// the first next call pulls the first page.
func NewDBCursorStream(deps DBCursorStreamDeps) *DBCursorStream {
	return &DBCursorStream{
		store:       deps.Store,
		backendName: deps.BackendName,
	}
}

// Next returns the next row from the DB cursor, fetching a fresh bounded page
// when the in-memory buffer drains. Returns (zero, false, nil) at
// end-of-stream, and never blocks on the DB once exhausted.
func (d *DBCursorStream) Next(ctx context.Context) (Entry, bool, error) {
	for {
		// Drain the in-memory page first.
		if d.idx < len(d.page) {
			row := d.page[d.idx]
			d.idx++
			d.cursor = row.ObjectKey
			return Entry{key: row.ObjectKey, size: row.SizeBytes}, true, nil
		}
		if d.exhausted {
			return Entry{}, false, nil
		}
		if err := ctx.Err(); err != nil {
			return Entry{}, false, err
		}
		rows, err := d.store.ListObjectsByBackendKeyAsc(ctx, d.backendName, d.cursor, dbCursorPageSize)
		if err != nil {
			return Entry{}, false, fmt.Errorf("page DB objects: %w", err)
		}
		if len(rows) == 0 {
			d.exhausted = true
			return Entry{}, false, nil
		}
		d.page = rows
		d.idx = 0
	}
}

// stop is a no-op for the DB cursor  -  the iterator owns no goroutine and
// holds no other resource that needs explicit teardown. Defined so the
// type satisfies keySource alongside S3KeyStream, which does need cleanup.
func (d *DBCursorStream) Stop() {
	// Intentionally empty: nothing to release. Required to satisfy the
	// keySource interface uniformly with S3KeyStream, whose stop tears
	// down a producer goroutine.
}

// -------------------------------------------------------------------------
// RECONCILE HANDLERS
// -------------------------------------------------------------------------

// ImportHandler returns the onImport callback used by the merge. Failures
// are logged but do not abort the reconcile pass  -  a single import
// failure should not stop the diff for thousands of other keys.
func ImportHandler(log *slog.Logger, backendName string, importer ImporterFn, result *Result) func(context.Context, Entry) error {
	return func(ctx context.Context, e Entry) error {
		outcome, err := importer(ctx, e.key, backendName, e.size, e.unmanaged)
		if err != nil {
			log.WarnContext(ctx, "import failed", "key", e.key, "backend", backendName, "error", err)
			return nil
		}
		switch outcome {
		case core.ImportInserted:
			result.Imported++
		case core.ImportSkippedPendingCleanup:
			log.WarnContext(ctx, "skipping key with an outstanding delete",
				"key", e.key, "backend", backendName)
			result.SuppressedPendingCleanup++
		}
		return nil
	}
}

// DeleteHandler returns the onDelete callback used by the merge. Failures
// are logged but do not abort the pass.
func DeleteHandler(log *slog.Logger, backendName string, deleter DeleterFn, result *Result) func(context.Context, string) error {
	return func(ctx context.Context, key string) error {
		if err := deleter(ctx, key, backendName); err != nil {
			log.WarnContext(ctx, "stale entry removal failed", "key", key, "backend", backendName, "error", err)
			return nil
		}
		log.InfoContext(ctx, "stale entry removed", "key", key, "backend", backendName)
		result.Removed++
		return nil
	}
}

// Result is the in-progress accumulator handed to the import /
// delete callbacks. Promoted to a struct so tests can assert on it
// without importing internal/worker.
type Result struct {
	Imported                 int64
	Removed                  int64
	SuppressedPendingCleanup int64
}

// ImporterFn imports a backend-listed key into the metadata store.
// A row that already existed is a benign no-op; a key whose delete is
// still outstanding is refused rather than imported. Carrier type so
// tests can substitute a fake importer.
type ImporterFn func(ctx context.Context, key, backendName string, size int64, unmanaged bool) (core.ImportOutcome, error)

// DeleterFn removes a metadata row whose backend confirmed it does not
// hold the key. Carrier type so tests can substitute a fake deleter.
type DeleterFn func(ctx context.Context, key, backendName string) error
