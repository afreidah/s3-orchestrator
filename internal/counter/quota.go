// -------------------------------------------------------------------------------
// QuotaTracker - Routing View Of What Each Backend Holds
//
// Author: Alex Freidah
//
// Answers one question: of these backends, which has the most room. That is a
// ranking, and a ranking is allowed to be out of date - naming the
// second-emptiest backend costs an uneven spread that the next refresh
// corrects, and nothing more.
//
// It deliberately does not answer whether a write fits. That question is
// settled by the statement that claims the space - the conditional insert of a
// pending intent or a replica - against live rows inside its own transaction,
// so every instance is judged against the same totals. Keeping the two apart is
// what lets this be a cheap snapshot without making admission approximate.
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

// QuotaTracker holds the per-backend occupancy snapshot placement ranks
// against, reloaded on the usage service's tick, plus what this instance has
// placed since that reload.
//
// The placed counter is what keeps spread spreading. Without it the snapshot
// does not move between reloads, every write in the interval ranks the
// candidates identically, and spread degenerates into pack until the next one.
// It is deliberately advisory: it informs an ordering, never a decision about
// whether a write fits, so it costs nothing to be approximate and is dropped
// wholesale when the snapshot it corrects is replaced.
//
// A backend absent from the snapshot ranks as having no room, so one that has
// not been read yet is passed over rather than preferred.
type QuotaTracker struct {
	baseline atomic.Pointer[map[string]core.BackendQuotaUsage]
	placed   *Registry[atomic.Int64]

	writeMu sync.Mutex // serializes the copy-on-write baseline swap; readers never take it
}

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// NewQuotaTracker creates a tracker with an empty snapshot and a placement
// counter per named backend.
func NewQuotaTracker(backendNames []string) *QuotaTracker {
	q := &QuotaTracker{placed: NewRegistry[atomic.Int64](backendNames...)}
	empty := make(map[string]core.BackendQuotaUsage)
	q.baseline.Store(&empty)
	return q
}

// NotePlacement records bytes this instance has just placed on a backend, so
// the next ranking sees them before the snapshot does. Called when a claim
// succeeds; nothing depends on it being exact.
func (q *QuotaTracker) NotePlacement(backend string, size int64) {
	if size == 0 {
		return
	}
	q.placed.Get(backend).Add(size)
}

// placedOn reports what this instance has put on a backend since the snapshot
// it is correcting was loaded.
func (q *QuotaTracker) placedOn(backend string) int64 {
	entry := q.placed.Peek(backend)
	if entry == nil {
		return 0
	}
	return entry.Load()
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
	// The rows just read already account for what this instance placed, so the
	// correction is spent and carrying it forward would double it.
	q.placed.SwapAll()
}

// Baseline returns one backend's snapshot and whether the tracker holds it.
func (q *QuotaTracker) Baseline(backend string) (core.BackendQuotaUsage, bool) {
	base, ok := (*q.baseline.Load())[backend]
	return base, ok
}

// -------------------------------------------------------------------------
// ROUTING
// -------------------------------------------------------------------------

// Available reports the bytes a backend looked able to accept as of the last
// refresh. An unlimited backend reports MaxInt64 so it sorts as the roomiest
// candidate without special-casing at every call site.
//
// Zero for a backend with no baseline, which is how an unprovisioned backend
// drops out of routing rather than being ranked ahead of one that exists.
func (q *QuotaTracker) Available(backend string) int64 {
	base, ok := q.Baseline(backend)
	if !ok {
		return 0
	}
	if base.Unlimited() {
		return math.MaxInt64
	}
	available := base.BytesLimit - base.Occupied() - q.placedOn(backend)
	if available < 0 {
		return 0
	}
	return available
}

// Utilization is the fraction of a backend's limit that was spoken for as of
// the last refresh. An unlimited backend reports zero so it ranks ahead of any
// bounded one, matching how the routing query ordered them.
func (q *QuotaTracker) Utilization(backend string) float64 {
	base, ok := q.Baseline(backend)
	if !ok || base.Unlimited() {
		return 0
	}
	return float64(base.Occupied()+q.placedOn(backend)) / float64(base.BytesLimit)
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
