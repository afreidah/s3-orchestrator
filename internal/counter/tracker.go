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
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// UsageTracker tracks per-backend usage counters, enforces monthly usage
// limits, and flushes accumulated deltas to the database. Counter storage
// is delegated to a Backend (local atomics or shared Redis). The
// limits and baseline maps live behind atomic.Pointer snapshots so reads
// require no locking and writes copy-on-write.
type UsageTracker struct {
	backend  Backend
	limits   atomic.Pointer[map[string]core.UsageLimits]
	baseline atomic.Pointer[map[string]core.UsageStat]

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
	return u
}

// Record increments the usage counters for a backend.
func (u *UsageTracker) Record(backendName string, apiCalls, egress, ingress int64) {
	u.backend.AddAll(backendName, apiCalls, egress, ingress)
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
func (u *UsageTracker) WithinLimits(backendName string, apiCalls, egress, ingress int64) bool {
	limits := *u.limits.Load()
	baseline := *u.baseline.Load()
	return withinLimitsSnapshot(u.backend, limits, baseline, backendName, apiCalls, egress, ingress)
}

// withinLimitsSnapshot is the snapshot-aware core of WithinLimits.
// BackendsWithinLimits loads the snapshots once and passes them in so
// every per-backend check sees a consistent view and the atomic Loads
// do not repeat per iteration.
func withinLimitsSnapshot(backend Backend, limits map[string]core.UsageLimits, baseline map[string]core.UsageStat, name string, apiCalls, egress, ingress int64) bool {
	lim, ok := limits[name]
	if !ok {
		return true // no limits configured
	}
	if lim.APIRequestLimit == 0 && lim.EgressByteLimit == 0 && lim.IngressByteLimit == 0 {
		return true // all unlimited
	}

	base := baseline[name]
	cur := backend.LoadAll(name)

	if lim.APIRequestLimit > 0 && base.APIRequests+cur.APIRequests+apiCalls > lim.APIRequestLimit {
		return false
	}
	if lim.EgressByteLimit > 0 && base.EgressBytes+cur.EgressBytes+egress > lim.EgressByteLimit {
		return false
	}
	if lim.IngressByteLimit > 0 && base.IngressBytes+cur.IngressBytes+ingress > lim.IngressByteLimit {
		return false
	}
	return true
}

// BackendsWithinLimits returns the subset of the given order whose
// backends are within their monthly usage limits for the proposed
// operation dimensions. Loads the limits and baseline snapshots ONCE
// so every per-backend check sees a consistent view; with the old
// RWMutex pair each WithinLimits call could see a different snapshot
// if a writer landed between iterations.
func (u *UsageTracker) BackendsWithinLimits(order []string, apiCalls, egress, ingress int64) []string {
	limits := *u.limits.Load()
	baseline := *u.baseline.Load()
	eligible := make([]string, 0, len(order))
	for _, name := range order {
		if withinLimitsSnapshot(u.backend, limits, baseline, name, apiCalls, egress, ingress) {
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
func (u *UsageTracker) SetBaseline(name string, stat core.UsageStat) {
	u.writeMu.Lock()
	defer u.writeMu.Unlock()
	src := *u.baseline.Load()
	cp := make(map[string]core.UsageStat, len(src)+1)
	maps.Copy(cp, src)
	cp[name] = stat
	u.baseline.Store(&cp)
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
}

// NearLimit returns true if any backend's effective usage (baseline +
// unflushed) exceeds the given threshold ratio for any non-zero limit
// dimension. Used by adaptive flushing to shorten the flush interval
// when enforcement accuracy matters. Loads both snapshots once so the
// loop sees a consistent view.
func (u *UsageTracker) NearLimit(threshold float64) bool {
	limits := *u.limits.Load()
	baseline := *u.baseline.Load()
	for name, lim := range limits {
		if isUnlimited(lim) {
			continue
		}
		base := baseline[name]
		cur := u.backend.LoadAll(name)
		if backendNearLimit(base, cur, lim, threshold) {
			return true
		}
	}
	return false
}

// isUnlimited returns true when all three usage dimensions are 0 (unlimited).
func isUnlimited(lim core.UsageLimits) bool {
	return lim.APIRequestLimit == 0 && lim.EgressByteLimit == 0 && lim.IngressByteLimit == 0
}

// backendNearLimit returns true when any configured dimension of the
// given backend has effective usage (baseline + unflushed) at or above
// the threshold ratio.
func backendNearLimit(base core.UsageStat, cur LoadAllResult, lim core.UsageLimits, threshold float64) bool {
	if lim.APIRequestLimit > 0 && float64(base.APIRequests+cur.APIRequests)/float64(lim.APIRequestLimit) >= threshold {
		return true
	}
	if lim.EgressByteLimit > 0 && float64(base.EgressBytes+cur.EgressBytes)/float64(lim.EgressByteLimit) >= threshold {
		return true
	}
	if lim.IngressByteLimit > 0 && float64(base.IngressBytes+cur.IngressBytes)/float64(lim.IngressByteLimit) >= threshold {
		return true
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

		if apiReqs == 0 && egress == 0 && ingress == 0 {
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
	}

	return lastErr
}

// Backend returns the underlying Backend (local or Redis).
func (u *UsageTracker) Backend() Backend {
	return u.backend
}
