// -------------------------------------------------------------------------------
// QuotaTracker - In-Memory Byte Reservations Against Backend Quotas
//
// Author: Alex Freidah
//
// Holds each backend's unflushed byte delta and answers the two questions the
// write path used to put to the database on every object: does this write fit,
// and which backend has the most room. Reservation is a compare-and-add against
// the backend's ceiling, so a write that would cross the limit is refused
// before it starts rather than by a conditional UPDATE inside its transaction.
//
// The baseline is the quota row as the last flush left it, held behind an
// atomic.Pointer snapshot so the reservation path loads it without locking.
// Deltas accumulate per backend and are drained by the flush service, which
// writes each backend's total once per interval instead of once per object.
// -------------------------------------------------------------------------------

package counter

import (
	"cmp"
	"math"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// QuotaTracker tracks per-backend byte deltas that have not yet reached
// backend_quotas, and enforces each backend's limit against the baseline plus
// those deltas.
//
// Enforcement is exact within one instance: a reservation that would cross the
// ceiling loses the compare-and-swap and is refused. Across a fleet each
// instance judges against its own unflushed delta, so the collective overshoot
// is bounded by what every other instance has reserved and not yet flushed.
type QuotaTracker struct {
	deltas   *Registry[atomic.Int64]
	baseline atomic.Pointer[map[string]core.BackendQuotaUsage]

	writeMu sync.Mutex // serializes the copy-on-write baseline swap; the reservation path never takes it
}

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// NewQuotaTracker creates a tracker with a delta counter per named backend and
// an empty baseline. Until baselines are set the tracker refuses every
// reservation, because a backend it has no row for is one it cannot prove has
// room - which is what the conditional UPDATE did with a missing row.
func NewQuotaTracker(backendNames []string) *QuotaTracker {
	q := &QuotaTracker{deltas: NewRegistry[atomic.Int64](backendNames...)}
	empty := make(map[string]core.BackendQuotaUsage)
	q.baseline.Store(&empty)
	return q
}

// -------------------------------------------------------------------------
// BASELINE
// -------------------------------------------------------------------------

// SetBaselines replaces the per-backend baseline snapshot. Called at startup
// and after every flush, with the rows as the flush left them.
//
// Copy-on-write: the map is swapped in whole so a reservation reads one
// consistent view rather than a row that changed under it mid-decision.
func (q *QuotaTracker) SetBaselines(baselines map[string]core.BackendQuotaUsage) {
	if baselines == nil {
		baselines = make(map[string]core.BackendQuotaUsage)
	}
	q.writeMu.Lock()
	defer q.writeMu.Unlock()
	q.baseline.Store(&baselines)
}

// Baseline returns one backend's snapshot and whether the tracker holds it.
func (q *QuotaTracker) Baseline(backend string) (core.BackendQuotaUsage, bool) {
	base, ok := (*q.baseline.Load())[backend]
	return base, ok
}

// -------------------------------------------------------------------------
// RESERVATION
// -------------------------------------------------------------------------

// Reserve claims size bytes against a backend, returning nil when they do not
// fit. A refused reservation changes nothing, so the caller is free to try the
// next candidate backend.
//
// The loop retries on a lost compare-and-swap rather than reading once and
// adding: two concurrent writes that each fit on their own must not both
// succeed when only one of them fits.
//
// The returned handle must be disposed of exactly once, by Commit when the
// write lands or Release when it does not. A leaked reservation is worse than
// a lost one: the flush writes it into bytes_used as though bytes had been
// stored, and the counter stays wrong until the next reconcile.
func (q *QuotaTracker) Reserve(backend string, size int64) *Reservation {
	base, ok := q.Baseline(backend)
	if !ok {
		return nil
	}
	entry := q.deltas.Get(backend)
	if base.Unlimited() {
		entry.Add(size)
		return &Reservation{tracker: q, backend: backend, size: size}
	}

	headroom := base.BytesLimit - base.Occupied()
	for {
		current := entry.Load()
		if current+size > headroom {
			return nil
		}
		if entry.CompareAndSwap(current, current+size) {
			return &Reservation{tracker: q, backend: backend, size: size}
		}
	}
}

// Apply folds a committed transaction's per-backend deltas into the counter,
// for the mutations that change byte totals without having reserved first:
// deletes, promotions of recovered intents, and replication.
func (q *QuotaTracker) Apply(deltas core.QuotaDeltas) {
	for name, delta := range deltas {
		q.Record(name, delta)
	}
}

// Record applies a signed delta without judging it against the limit, for the
// paths that change a backend's byte total without asking permission: deletes,
// cleanup of displaced copies, and replication accounting for bytes that are
// already on the backend.
func (q *QuotaTracker) Record(backend string, delta int64) {
	if delta == 0 {
		return
	}
	q.deltas.Get(backend).Add(delta)
}

// -------------------------------------------------------------------------
// RESERVATION HANDLE
// -------------------------------------------------------------------------

// Reservation is bytes claimed on one backend by a write that has not finished.
// It exists so disposal is a single deferred call at the site that took it,
// rather than an obligation every error path has to remember.
//
// A nil Reservation is the refused one, and both methods accept it, so a caller
// can defer Release on the result of Reserve before deciding whether it got
// anything.
type Reservation struct {
	tracker  *QuotaTracker
	backend  string
	size     int64
	disposed atomic.Bool
}

// Backend names the backend the bytes were claimed on.
func (r *Reservation) Backend() string {
	if r == nil {
		return ""
	}
	return r.backend
}

// Commit replaces the claim with what the transaction actually committed. The
// deltas carry the write's own bytes as the ledger recorded them, so the claim
// is dropped in the same breath rather than counted twice.
//
// Deltas land before the claim is dropped: for the moment between them the
// backend looks fuller than it is, which refuses a concurrent write that would
// have fit rather than admitting one that would not.
func (r *Reservation) Commit(deltas core.QuotaDeltas) {
	if r == nil || !r.disposed.CompareAndSwap(false, true) {
		return
	}
	r.tracker.Apply(deltas)
	r.tracker.Record(r.backend, -r.size)
}

// Release returns the claim when the write did not land. Safe to defer at the
// point of reservation: a reservation already committed is left alone.
func (r *Reservation) Release() {
	if r == nil || !r.disposed.CompareAndSwap(false, true) {
		return
	}
	r.tracker.Record(r.backend, -r.size)
}

// -------------------------------------------------------------------------
// ROUTING
// -------------------------------------------------------------------------

// Available reports the bytes a backend can still accept: its limit less what
// the baseline says it holds and what this instance has reserved since. An
// unlimited backend reports MaxInt64 so it sorts as the roomiest candidate
// without special-casing at every call site.
//
// Zero for a backend with no baseline, which is how an unprovisioned backend
// drops out of routing rather than being selected and failing at write time.
func (q *QuotaTracker) Available(backend string) int64 {
	base, ok := q.Baseline(backend)
	if !ok {
		return 0
	}
	if base.Unlimited() {
		return math.MaxInt64
	}
	available := base.BytesLimit - base.Occupied() - q.Delta(backend)
	if available < 0 {
		return 0
	}
	return available
}

// Utilization is the fraction of a backend's limit that is spoken for,
// baseline plus unflushed delta. An unlimited backend reports zero so it ranks
// ahead of any bounded one, matching how the routing query ordered them.
func (q *QuotaTracker) Utilization(backend string) float64 {
	base, ok := q.Baseline(backend)
	if !ok || base.Unlimited() {
		return 0
	}
	return float64(base.Occupied()+q.Delta(backend)) / float64(base.BytesLimit)
}

// RankByUtilization returns the candidates ordered emptiest first, on a copy so
// the caller's slice keeps its order. The single ordering every placement
// decision that spreads load reads from - the write path's spread strategy and
// drain's choice of where to move a copy - so the two cannot disagree about
// which backend is emptiest.
func (q *QuotaTracker) RankByUtilization(candidates []string) []string {
	ranked := slices.Clone(candidates)
	slices.SortStableFunc(ranked, func(a, b string) int {
		return cmp.Compare(q.Utilization(a), q.Utilization(b))
	})
	return ranked
}

// Delta returns the bytes reserved against a backend since the last flush.
func (q *QuotaTracker) Delta(backend string) int64 {
	entry := q.deltas.Peek(backend)
	if entry == nil {
		return 0
	}
	return entry.Load()
}

// -------------------------------------------------------------------------
// FLUSH
// -------------------------------------------------------------------------

// SwapDeltas reads and resets every backend's delta in one swap, returning the
// totals the flush must write. Backends whose delta is zero are left out, so a
// quiet backend costs no UPDATE.
func (q *QuotaTracker) SwapDeltas() map[string]int64 {
	old := q.deltas.SwapAll()
	out := make(map[string]int64, len(old))
	for name, entry := range old {
		if delta := entry.Load(); delta != 0 {
			out[name] = delta
		}
	}
	return out
}

// RestoreDeltas adds unwritten deltas back after a failed flush, so the next
// pass carries what this one could not. Restoring rather than discarding is
// what keeps bytes_used from silently losing a flush interval of writes.
func (q *QuotaTracker) RestoreDeltas(deltas map[string]int64) {
	for name, delta := range deltas {
		q.Record(name, delta)
	}
}
