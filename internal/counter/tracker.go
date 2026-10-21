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
	"cmp"
	"context"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// UsageTracker tracks per-backend usage counters, enforces monthly usage
// limits, and flushes accumulated deltas to the database. Counter storage
// is delegated to a Backend (local atomics or shared Redis). The
// limits and baseline maps live behind atomic.Pointer snapshots so reads
// require no locking; each write replaces a whole map.
type UsageTracker struct {
	backend  Backend
	limits   atomic.Pointer[map[string]core.UsageLimits]
	baseline atomic.Pointer[map[string]core.Usage]
}

// NewUsageTracker creates a usage tracker with the given counter backend
// and per-backend limits. The counter backend determines whether deltas
// are stored locally (default) or in a shared store like Redis.
func NewUsageTracker(backend Backend, limits map[string]core.UsageLimits) *UsageTracker {
	u := &UsageTracker{backend: backend}
	u.UpdateLimits(limits)
	u.SetBaselines(nil)
	return u
}

// Record increments the usage counters for a backend under ctx's workload.
// Operations no pool charges are still counted in api_requests.
func (u *UsageTracker) Record(ctx context.Context, backendName string, op s3op.Operation, egress, ingress int64) {
	u.record(ctx, backendName, []s3op.Operation{op}, 1, egress, ingress)
}

// RecordN credits n calls of the same operation, for the paths that make a
// run of identical requests before reporting: a paginated list walks a bucket
// one page at a time and charges for every page.
func (u *UsageTracker) RecordN(ctx context.Context, backendName string, op s3op.Operation, n int64) {
	u.record(ctx, backendName, []s3op.Operation{op}, n, 0, 0)
}

// record is the single charging path: each operation counts once per
// repetition against the backend's request total, and once per repetition
// against every pool that contains it, all in one charge to ctx's workload.
func (u *UsageTracker) record(ctx context.Context, backendName string, ops []s3op.Operation, each, egress, ingress int64) {
	if len(ops) == 0 || each <= 0 {
		return
	}
	c := Charge{
		Workload:     string(cmp.Or(audit.WorkloadOf(ctx), audit.WorkloadUnknown)),
		APIRequests:  int64(len(ops)) * each,
		EgressBytes:  egress,
		IngressBytes: ingress,
		PoolN:        each,
	}
	lim := (*u.limits.Load())[backendName]
	for i, op := range ops {
		pools := lim.PoolsFor(op)
		if i == 0 {
			// The limits' own slice, so a single operation charges without
			// allocating; Clip makes a later append copy rather than write
			// into it.
			c.Pools = slices.Clip(pools)
			continue
		}
		c.Pools = append(c.Pools, pools...)
	}
	u.backend.Charge(backendName, c)
}

// poolCharge counts how many times the operations charge the named pool. A
// pool named by two of the operations is charged twice.
func poolCharge(lim core.UsageLimits, ops []s3op.Operation, pool string) int64 {
	var n int64
	for _, op := range ops {
		for _, p := range lim.PoolsFor(op) {
			if p.Name == pool {
				n++
			}
		}
	}
	return n
}

// -------------------------------------------------------------------------
// LIMIT ENFORCEMENT
// -------------------------------------------------------------------------

// WithinLimits reports whether baseline + unflushed + proposed stays under every
// non-zero monthly limit for the backend. It takes no lock, so concurrent
// requests can overshoot by up to one flush interval of traffic.
func (u *UsageTracker) WithinLimits(backendName string, ops []s3op.Operation, egress, ingress int64) bool {
	return withinLimitsSnapshot(u.backend, u.snapshot(), backendName, ops, egress, ingress)
}

// usageSnapshot is one consistent view of what admission is judged against:
// the configured limits, and the flushed baselines they are compared to. The
// two are loaded together because a decision assembled from two different
// snapshots - limits reloaded between dimensions, say - is a decision made
// against a configuration that never existed.
type usageSnapshot struct {
	limits    map[string]core.UsageLimits
	baselines map[string]core.Usage
}

// snapshot loads the copy-on-write views in one go. Callers that check
// several backends take it once so every per-backend decision sees the same
// view and the atomic loads do not repeat per iteration.
func (u *UsageTracker) snapshot() usageSnapshot {
	return usageSnapshot{limits: *u.limits.Load(), baselines: *u.baseline.Load()}
}

// unflushedPool recycles the values the limit checks read the unflushed
// counters into, keeping their pool maps, so a check on every request does not
// allocate.
var unflushedPool = sync.Pool{New: func() any { return new(core.Usage) }}

// withUsed calls fn with a backend's usage so far this period, as the flushed
// baseline and what is charged and not yet flushed, and returns fn's answer.
// The two are kept apart rather than summed, which would build a new pool
// map on every call.
func (s usageSnapshot) withUsed(backend Backend, name string, fn func(base core.Usage, cur *core.Usage) bool) bool {
	cur := unflushedPool.Get().(*core.Usage)
	defer unflushedPool.Put(cur)
	cur.Reset()
	backend.Load(name, cur)
	return fn(s.baselines[name], cur)
}

// withinLimitsSnapshot is the snapshot-aware core of WithinLimits. A pool
// charged by two of the operations must fit both, which is why the proposed
// charges are folded before they are compared.
func withinLimitsSnapshot(backend Backend, snap usageSnapshot, name string, ops []s3op.Operation, egress, ingress int64) bool {
	lim, ok := snap.limits[name]
	if !ok || lim.Unlimited() {
		return true
	}
	return snap.withUsed(backend, name, func(base core.Usage, cur *core.Usage) bool {
		if lim.EgressByteLimit > 0 && base.EgressBytes+cur.EgressBytes+egress > lim.EgressByteLimit {
			return false
		}
		if lim.IngressByteLimit > 0 && base.IngressBytes+cur.IngressBytes+ingress > lim.IngressByteLimit {
			return false
		}
		return poolsFit(lim, ops, base, cur)
	})
}

// poolsFit reports whether every bounded pool the operations charge has room
// for them. A pool with no limit is counted and never refused.
func poolsFit(lim core.UsageLimits, ops []s3op.Operation, base core.Usage, cur *core.Usage) bool {
	for _, pool := range lim.Pools() {
		charge := poolCharge(lim, ops, pool.Name)
		if pool.Limit > 0 && charge > 0 && base.Pools[pool.Name]+cur.Pools[pool.Name]+charge > pool.Limit {
			return false
		}
	}
	return true
}

// BackendsWithinLimits returns the subset of the given order whose
// backends are within their monthly usage limits for the proposed
// operation dimensions. It loads the limits and baseline snapshots once so
// every per-backend check sees a consistent view.
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

// SetBaselines replaces every backend's flushed usage for the current period
// in one swap. A backend absent from baselines has none, which is how a new
// period starts at zero.
func (u *UsageTracker) SetBaselines(baselines map[string]core.Usage) {
	if baselines == nil {
		baselines = make(map[string]core.Usage)
	}
	u.baseline.Store(&baselines)
}

// NearLimit returns true if any backend's effective usage (baseline +
// unflushed) exceeds the given threshold ratio for any non-zero limit
// dimension. Used by adaptive flushing to shorten the flush interval
// when enforcement accuracy matters. Takes the snapshot once so the loop sees
// a consistent view.
func (u *UsageTracker) NearLimit(threshold float64) bool {
	snap := u.snapshot()
	for name, lim := range snap.limits {
		if !lim.Unlimited() && snap.withUsed(u.backend, name, func(base core.Usage, cur *core.Usage) bool {
			return nearLimit(base, cur, lim, threshold)
		}) {
			return true
		}
	}
	return false
}

// nearLimit reports whether any bounded dimension of a backend's usage has
// reached the threshold ratio. One tight pool is enough to want a shorter
// flush interval, since that is the budget about to start refusing work.
func nearLimit(base core.Usage, cur *core.Usage, lim core.UsageLimits, threshold float64) bool {
	reached := func(n, limit int64) bool {
		return limit > 0 && float64(n)/float64(limit) >= threshold
	}
	if reached(base.EgressBytes+cur.EgressBytes, lim.EgressByteLimit) ||
		reached(base.IngressBytes+cur.IngressBytes, lim.IngressByteLimit) {
		return true
	}
	for _, pool := range lim.Pools() {
		if reached(base.Pools[pool.Name]+cur.Pools[pool.Name], pool.Limit) {
			return true
		}
	}
	return false
}

// -------------------------------------------------------------------------
// FLUSH
// -------------------------------------------------------------------------

// periodLayout is the time layout of a usage period, one calendar month.
const periodLayout = "2006-01"

// monthCache is the formatted current month and the instant it stops being
// current.
type monthCache struct {
	period string
	until  time.Time
}

// currentMonth caches CurrentPeriod's answer, which every charge and limit
// check asks for, so formatting it does not allocate on the request path.
var currentMonth atomic.Pointer[monthCache]

// CurrentPeriod returns the current month as "YYYY-MM" for usage
// aggregation.
func CurrentPeriod() string {
	now := time.Now()
	if c := currentMonth.Load(); c != nil && now.Before(c.until) {
		return c.period
	}
	first := monthStart(now)
	c := &monthCache{period: first.Format(periodLayout), until: first.AddDate(0, 1, 0)}
	currentMonth.Store(c)
	return c.period
}

// monthStart returns the first instant of t's month in UTC.
func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// flushPeriods returns the current and the previous month. A flush takes both,
// so charges made just before a rollover are flushed into the month they
// belong to after it.
func flushPeriods() []string {
	first := monthStart(time.Now())
	return []string{first.Format(periodLayout), first.AddDate(0, -1, 0).Format(periodLayout)}
}

// FlushUsage writes every batch of counters the backend hands over to the
// store: this instance's own, and with a shared backend the staged shared
// ones too. Called on every instance on the usage flush tick. A batch the
// store does not apply is kept by the backend for the next flush.
func (u *UsageTracker) FlushUsage(ctx context.Context, store core.UsageFlusher) error {
	batches := u.backend.Take(ctx)
	if shared, ok := u.backend.(SharedBackend); ok {
		batches = append(batches, shared.TakeShared(ctx)...)
	}
	var lastErr error
	for i := range batches {
		b := &batches[i]
		err := store.ApplyUsageBatch(ctx, b)
		u.backend.Settle(ctx, b, err == nil)
		if err != nil {
			slog.ErrorContext(ctx, "usage flush failed",
				logfmt.Component("usage_tracker"),
				slog.String("backend", b.Backend),
				slog.String("period", b.Period),
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
