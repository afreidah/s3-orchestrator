// -------------------------------------------------------------------------------
// UsageTracker - Per-Backend Usage Counting and Limit Enforcement
//
// Author: Alex Freidah
//
// Tracks in-memory usage counters (API requests, egress, ingress) per backend,
// enforces configurable monthly usage limits, and periodically flushes deltas to
// PostgreSQL. Counters are keyed by calendar month for automatic period rollover.
//
// The limits map and baseline map are held behind atomic.Pointer snapshots
// (copy-on-write): read paths do a single atomic load with zero locking and
// then operate on the immutable map; write paths take a small mutex,
// allocate a new map with the change, and atomically swap. This removes
// the RWMutex pair from every WithinLimits call (which fires on every
// backend operation) and lets BackendsWithinLimits / NearLimit observe a
// consistent snapshot across the loop instead of re-locking per backend.
// -------------------------------------------------------------------------------

package counter

import (
	"context"
	"log/slog"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// UsageTracker tracks per-backend usage counters, enforces monthly usage
// limits, and flushes accumulated deltas to the database. Counter storage
// is delegated to a Backend (local atomics or shared Redis). The
// limits and baseline maps live behind atomic.Pointer snapshots so reads
// require no locking and writes copy-on-write.
type UsageTracker struct {
	backend      Backend
	limits       atomic.Pointer[map[string]core.UsageLimits]
	baseline     atomic.Pointer[map[string]core.UsageStat]
	poolBaseline atomic.Pointer[map[string]core.PoolUsage]

	// writeMu serializes the copy-on-write swaps for limits and baseline
	// so two concurrent writers cannot lose each other's update. The hot
	// read path never touches this mutex.
	writeMu sync.Mutex
}

// NewUsageTracker creates a usage tracker with the given counter backend
// and per-backend limits. The counter backend determines whether deltas
// are stored locally (default) or in a shared store like Redis.
func NewUsageTracker(backend Backend, limits map[string]core.UsageLimits) *UsageTracker {
	if limits == nil {
		limits = make(map[string]core.UsageLimits)
	}
	u := &UsageTracker{backend: backend}
	u.limits.Store(&limits)
	emptyBaseline := make(map[string]core.UsageStat)
	u.baseline.Store(&emptyBaseline)
	emptyPools := make(map[string]core.PoolUsage)
	u.poolBaseline.Store(&emptyPools)
	return u
}

// Record increments the usage counters for a backend.
//
// Every operation is recorded here, including ones no pool charges: not
// billing an operation is not a reason to stop reporting that it happened,
// and api_requests stays the honest count of calls made.
func (u *UsageTracker) Record(backendName string, op s3op.Operation, egress, ingress int64) {
	u.record(backendName, []s3op.Operation{op}, 1, egress, ingress)
}

// RecordN credits n calls of the same operation, for the paths that make a
// run of identical requests before reporting: a paginated list walks a bucket
// one page at a time and charges for every page.
func (u *UsageTracker) RecordN(backendName string, op s3op.Operation, n int64) {
	u.record(backendName, []s3op.Operation{op}, n, 0, 0)
}

// RecordAll credits a mixed set of operations in one pass, for the paths that
// make more than one kind of call per unit of work: a rewrite reads an object
// and writes it back, and the two are charged to different pools.
func (u *UsageTracker) RecordAll(backendName string, ops []s3op.Operation, egress, ingress int64) {
	u.record(backendName, ops, 1, egress, ingress)
}

// record is the single charging path: each operation counts once per
// repetition against the backend's request total, and once per repetition
// against every pool that contains it.
func (u *UsageTracker) record(backendName string, ops []s3op.Operation, each, egress, ingress int64) {
	if len(ops) == 0 || each <= 0 {
		return
	}
	u.backend.AddAll(backendName, int64(len(ops))*each, egress, ingress)

	limits := *u.limits.Load()
	if deltas := poolDeltas(limits[backendName], ops, each); len(deltas) > 0 {
		u.backend.AddPools(backendName, deltas)
	}
}

// poolDeltas folds operations into the per-pool charges they incur. An
// operation charges every pool that contains it, so a pool named by two of
// the operations is charged twice.
func poolDeltas(lim core.UsageLimits, ops []s3op.Operation, each int64) map[string]int64 {
	var deltas map[string]int64
	for _, op := range ops {
		for _, pool := range lim.PoolsFor(op) {
			if deltas == nil {
				deltas = make(map[string]int64, len(ops))
			}
			deltas[pool.Name] += each
		}
	}
	return deltas
}

// -------------------------------------------------------------------------
// LIMIT ENFORCEMENT
// -------------------------------------------------------------------------

// WithinLimits checks whether the proposed operation would keep the given
// backend within its configured monthly usage limits. It computes:
//
//	effective = baseline (from DB) + unflushed counter + proposed
//
// Returns true if no non-zero limit is exceeded.
//
// Enforcement is approximate: the snapshot pair (limits, baseline) is
// read separately from the live counter, so concurrent requests may all
// pass the check and collectively exceed the limit by a small margin.
// This is intentional - exact enforcement would require a mutex on every
// request. The overshoot is bounded by one flush interval worth of
// concurrent traffic, and s3o_usage_limit_rejections_total tracks when
// limits are actively enforced.
func (u *UsageTracker) WithinLimits(backendName string, ops []s3op.Operation, egress, ingress int64) bool {
	return withinLimitsSnapshot(u.backend, u.snapshot(), backendName, ops, egress, ingress)
}

// usageSnapshot is one consistent view of what admission is judged against:
// the configured limits, and the flushed baselines they are compared to. The
// three are loaded together because a decision assembled from two different
// snapshots - limits reloaded between dimensions, say - is a decision made
// against a configuration that never existed.
type usageSnapshot struct {
	limits map[string]core.UsageLimits
	stats  map[string]core.UsageStat
	pools  map[string]core.PoolUsage
}

// snapshot loads the three copy-on-write views in one go. Callers that check
// several backends take it once so every per-backend decision sees the same
// view and the atomic loads do not repeat per iteration.
func (u *UsageTracker) snapshot() usageSnapshot {
	return usageSnapshot{
		limits: *u.limits.Load(),
		stats:  *u.baseline.Load(),
		pools:  *u.poolBaseline.Load(),
	}
}

// withinLimitsSnapshot is the snapshot-aware core of WithinLimits.
func withinLimitsSnapshot(backend Backend, snap usageSnapshot, name string, ops []s3op.Operation, egress, ingress int64) bool {
	lim, ok := snap.limits[name]
	if !ok {
		return true // no limits configured
	}
	if lim.Unlimited() {
		return true
	}

	base := snap.stats[name]
	cur := backend.LoadAll(name)

	if lim.EgressByteLimit > 0 && base.EgressBytes+cur.EgressBytes+egress > lim.EgressByteLimit {
		return false
	}
	if lim.IngressByteLimit > 0 && base.IngressBytes+cur.IngressBytes+ingress > lim.IngressByteLimit {
		return false
	}
	return poolsWithinLimits(backend, lim, snap.pools[name], name, ops)
}

// poolsWithinLimits reports whether every pool the operations charge has room
// for them. A pool charged by two of the operations must fit both, which is
// why the proposed charges are folded before they are compared.
func poolsWithinLimits(backend Backend, lim core.UsageLimits, base core.PoolUsage, name string, ops []s3op.Operation) bool {
	proposed := poolDeltas(lim, ops, 1)
	for _, pool := range lim.Pools() {
		charge := proposed[pool.Name]
		if charge == 0 || pool.Limit <= 0 {
			continue // untouched by these operations, or counted and never refused
		}
		if base[pool.Name]+backend.LoadPool(name, pool.Name)+charge > pool.Limit {
			return false
		}
	}
	return true
}

// BackendsWithinLimits returns the subset of the given order whose
// backends are within their monthly usage limits for the proposed
// operation dimensions. Loads the limits and baseline snapshots ONCE
// so every per-backend check sees a consistent view; with the old
// RWMutex pair each WithinLimits call could see a different snapshot
// if a writer landed between iterations.
func (u *UsageTracker) BackendsWithinLimits(order []string, ops []s3op.Operation, egress, ingress int64) []string {
	snap := u.snapshot()
	eligible := make([]string, 0, len(order))
	for _, name := range order {
		if withinLimitsSnapshot(u.backend, snap, name, ops, egress, ingress) {
			eligible = append(eligible, name)
		}
	}
	return eligible
}

// -------------------------------------------------------------------------
// CONFIGURATION
// -------------------------------------------------------------------------

// UpdateLimits replaces the per-backend usage limits via copy-on-write
// swap. Safe to call concurrently with request handling: in-flight
// WithinLimits calls keep using whichever snapshot they loaded.
func (u *UsageTracker) UpdateLimits(limits map[string]core.UsageLimits) {
	u.writeMu.Lock()
	defer u.writeMu.Unlock()
	cp := make(map[string]core.UsageLimits, len(limits))
	maps.Copy(cp, limits)
	u.limits.Store(&cp)
}

// GetLimits returns a shallow copy of the current per-backend usage
// limits. The snapshot itself is immutable, but a defensive copy keeps
// the API contract from leaking the live snapshot to callers that
// might mutate it.
func (u *UsageTracker) GetLimits() map[string]core.UsageLimits {
	src := *u.limits.Load()
	cp := make(map[string]core.UsageLimits, len(src))
	maps.Copy(cp, src)
	return cp
}

// -------------------------------------------------------------------------
// BASELINE MANAGEMENT
// -------------------------------------------------------------------------

// SetBaseline updates the cached DB usage baseline for a single backend
// via copy-on-write swap.
func (u *UsageTracker) SetBaseline(name string, stat core.UsageStat, pools core.PoolUsage) {
	u.writeMu.Lock()
	defer u.writeMu.Unlock()

	src := *u.baseline.Load()
	cp := make(map[string]core.UsageStat, len(src)+1)
	maps.Copy(cp, src)
	cp[name] = stat
	u.baseline.Store(&cp)

	poolSrc := *u.poolBaseline.Load()
	poolCp := make(map[string]core.PoolUsage, len(poolSrc)+1)
	maps.Copy(poolCp, poolSrc)
	poolCp[name] = pools
	u.poolBaseline.Store(&poolCp)
}

// ResetBaselines zeroes out the baseline for the given backend names
// via copy-on-write swap.
func (u *UsageTracker) ResetBaselines(names []string) {
	u.writeMu.Lock()
	defer u.writeMu.Unlock()
	src := *u.baseline.Load()
	cp := make(map[string]core.UsageStat, len(src))
	maps.Copy(cp, src)
	for _, name := range names {
		cp[name] = core.UsageStat{}
	}
	u.baseline.Store(&cp)

	poolSrc := *u.poolBaseline.Load()
	poolCp := make(map[string]core.PoolUsage, len(poolSrc))
	maps.Copy(poolCp, poolSrc)
	for _, name := range names {
		poolCp[name] = nil
	}
	u.poolBaseline.Store(&poolCp)
}

// NearLimit returns true if any backend's effective usage (baseline +
// unflushed) exceeds the given threshold ratio for any non-zero limit
// dimension. Used by adaptive flushing to shorten the flush interval
// when enforcement accuracy matters. Takes the snapshot once so the loop sees
// a consistent view.
func (u *UsageTracker) NearLimit(threshold float64) bool {
	snap := u.snapshot()
	for name, lim := range snap.limits {
		if lim.Unlimited() {
			continue
		}
		base := snap.stats[name]
		cur := u.backend.LoadAll(name)
		if backendNearLimit(base, cur, lim, threshold) {
			return true
		}
		if u.poolNearLimit(name, lim, snap.pools[name], threshold) {
			return true
		}
	}
	return false
}

// backendNearLimit returns true when either byte dimension of the given
// backend has effective usage (baseline + unflushed) at or above the
// threshold ratio.
func backendNearLimit(base core.UsageStat, cur LoadAllResult, lim core.UsageLimits, threshold float64) bool {
	if lim.EgressByteLimit > 0 && float64(base.EgressBytes+cur.EgressBytes)/float64(lim.EgressByteLimit) >= threshold {
		return true
	}
	if lim.IngressByteLimit > 0 && float64(base.IngressBytes+cur.IngressBytes)/float64(lim.IngressByteLimit) >= threshold {
		return true
	}
	return false
}

// poolNearLimit returns true when any bounded request pool on the backend has
// reached the threshold ratio. One tight pool is enough to want a shorter
// flush interval, since that is the budget about to start refusing work.
func (u *UsageTracker) poolNearLimit(name string, lim core.UsageLimits, base core.PoolUsage, threshold float64) bool {
	for _, pool := range lim.Pools() {
		if pool.Limit <= 0 {
			continue
		}
		used := base[pool.Name] + u.backend.LoadPool(name, pool.Name)
		if float64(used)/float64(pool.Limit) >= threshold {
			return true
		}
	}
	return false
}

// -------------------------------------------------------------------------
// FLUSH
// -------------------------------------------------------------------------

// CurrentPeriod returns the current month as "YYYY-MM" for usage
// aggregation.
func CurrentPeriod() string {
	return time.Now().UTC().Format("2006-01")
}

// usageFlusher is the consumer-defined slice of the metadata store
// FlushUsage actually calls. Declared here so the counter package owns
// its own dependency contract instead of importing a producer-side
// type from internal/store/core.
type usageFlusher interface {
	FlushUsageDeltas(ctx context.Context, backendName, period string, apiRequests, egressBytes, ingressBytes int64) error
	FlushPoolDeltas(ctx context.Context, backendName, period string, deltas core.PoolUsage) error
}

// FlushUsage reads and resets the counter backend, then writes the
// accumulated deltas to the database. Called periodically (every 30s).
// On DB error, deltas are added back to avoid data loss. Backends in
// the skip set have their counters discarded (used for drained
// backends whose DB records are gone).
func (u *UsageTracker) FlushUsage(ctx context.Context, store usageFlusher, skip map[string]bool) error {
	period := CurrentPeriod()
	var lastErr error

	for _, name := range u.backend.Backends() {
		apiReqs := u.backend.Swap(name, FieldAPIRequests)
		egress := u.backend.Swap(name, FieldEgressBytes)
		ingress := u.backend.Swap(name, FieldIngressBytes)
		pools := u.backend.SwapPools(name)

		if apiReqs == 0 && egress == 0 && ingress == 0 && len(pools) == 0 {
			continue
		}

		if skip[name] {
			continue // discard -- DB records for this backend are gone
		}

		if err := store.FlushUsageDeltas(ctx, name, period, apiReqs, egress, ingress); err != nil {
			// Restore deltas so they aren't lost
			u.backend.Add(name, FieldAPIRequests, apiReqs)
			u.backend.Add(name, FieldEgressBytes, egress)
			u.backend.Add(name, FieldIngressBytes, ingress)
			slog.ErrorContext(ctx, "usage delta flush failed",
				logfmt.Component("usage_tracker"),
				slog.String("backend", name),
				"error", err,
			)
			lastErr = err
		}

		// Flushed separately from the totals: a pool delta restored into the
		// counter after the totals landed would be double-counted on the next
		// pass, so each half restores only what it failed to write.
		if len(pools) == 0 {
			continue
		}
		if err := store.FlushPoolDeltas(ctx, name, period, pools); err != nil {
			u.backend.AddPools(name, pools)
			slog.ErrorContext(ctx, "request pool delta flush failed",
				logfmt.Component("usage_tracker"),
				slog.String("backend", name),
				"error", err,
			)
			lastErr = err
		}
	}

	return lastErr
}

// Backend returns the underlying Backend (local or Redis).
func (u *UsageTracker) Backend() Backend {
	return u.backend
}
