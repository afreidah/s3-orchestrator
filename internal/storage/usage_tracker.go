// -------------------------------------------------------------------------------
// UsageTracker - Per-Backend Usage Counting and Limit Enforcement
//
// Author: Alex Freidah
//
// Tracks in-memory usage counters (API requests, egress, ingress) per backend,
// enforces configurable monthly usage limits, and periodically flushes deltas to
// PostgreSQL. Counters are keyed by calendar month for automatic period rollover.
// -------------------------------------------------------------------------------

package storage

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// UsageTracker tracks per-backend usage counters, enforces monthly usage limits,
// and flushes accumulated deltas to the database. Counter storage is delegated
// to a CounterBackend, allowing local atomics or shared Redis counters.
type UsageTracker struct {
	backend    CounterBackend
	limits     map[string]UsageLimits
	limitsMu   sync.RWMutex
	baseline   map[string]UsageStat
	baselineMu sync.RWMutex
}

// NewUsageTracker creates a usage tracker with the given counter backend and
// per-backend limits. The counter backend determines whether deltas are stored
// locally (default) or in a shared store like Redis.
func NewUsageTracker(backend CounterBackend, limits map[string]UsageLimits) *UsageTracker {
	if limits == nil {
		limits = make(map[string]UsageLimits)
	}
	return &UsageTracker{
		backend:  backend,
		limits:   limits,
		baseline: make(map[string]UsageStat),
	}
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
func (u *UsageTracker) WithinLimits(backendName string, apiCalls, egress, ingress int64) bool {
	u.limitsMu.RLock()
	lim, ok := u.limits[backendName]
	u.limitsMu.RUnlock()
	if !ok {
		return true // no limits configured
	}
	if lim.APIRequestLimit == 0 && lim.EgressByteLimit == 0 && lim.IngressByteLimit == 0 {
		return true // all unlimited
	}

	u.baselineMu.RLock()
	base := u.baseline[backendName]
	u.baselineMu.RUnlock()

	cur := u.backend.LoadAll(backendName)

	if lim.APIRequestLimit > 0 {
		if base.APIRequests+cur.APIRequests+apiCalls > lim.APIRequestLimit {
			return false
		}
	}
	if lim.EgressByteLimit > 0 {
		if base.EgressBytes+cur.EgressBytes+egress > lim.EgressByteLimit {
			return false
		}
	}
	if lim.IngressByteLimit > 0 {
		if base.IngressBytes+cur.IngressBytes+ingress > lim.IngressByteLimit {
			return false
		}
	}
	return true
}

// BackendsWithinLimits returns the subset of the given order whose backends are
// within their monthly usage limits for the proposed operation dimensions.
func (u *UsageTracker) BackendsWithinLimits(order []string, apiCalls, egress, ingress int64) []string {
	eligible := make([]string, 0, len(order))
	for _, name := range order {
		if u.WithinLimits(name, apiCalls, egress, ingress) {
			eligible = append(eligible, name)
		}
	}
	return eligible
}

// -------------------------------------------------------------------------
// CONFIGURATION
// -------------------------------------------------------------------------

// UpdateLimits replaces the per-backend usage limits. Safe to call concurrently
// with request handling.
func (u *UsageTracker) UpdateLimits(limits map[string]UsageLimits) {
	u.limitsMu.Lock()
	defer u.limitsMu.Unlock()
	u.limits = limits
}

// GetLimits returns a shallow copy of the current per-backend usage limits.
func (u *UsageTracker) GetLimits() map[string]UsageLimits {
	u.limitsMu.RLock()
	defer u.limitsMu.RUnlock()
	cp := make(map[string]UsageLimits, len(u.limits))
	for k, v := range u.limits {
		cp[k] = v
	}
	return cp
}

// -------------------------------------------------------------------------
// BASELINE MANAGEMENT
// -------------------------------------------------------------------------

// SetBaseline updates the cached DB usage baseline for a single backend.
func (u *UsageTracker) SetBaseline(name string, stat UsageStat) {
	u.baselineMu.Lock()
	defer u.baselineMu.Unlock()
	u.baseline[name] = stat
}

// ResetBaselines zeroes out all baselines for the given backend names.
func (u *UsageTracker) ResetBaselines(names []string) {
	u.baselineMu.Lock()
	defer u.baselineMu.Unlock()
	for _, name := range names {
		u.baseline[name] = UsageStat{}
	}
}

// NearLimit returns true if any backend's effective usage (baseline + unflushed)
// exceeds the given threshold ratio for any non-zero limit dimension. Used by
// adaptive flushing to shorten the flush interval when enforcement accuracy matters.
func (u *UsageTracker) NearLimit(threshold float64) bool {
	u.limitsMu.RLock()
	defer u.limitsMu.RUnlock()

	u.baselineMu.RLock()
	defer u.baselineMu.RUnlock()

	for name, lim := range u.limits {
		if lim.APIRequestLimit == 0 && lim.EgressByteLimit == 0 && lim.IngressByteLimit == 0 {
			continue
		}

		base := u.baseline[name]
		cur := u.backend.LoadAll(name)

		if lim.APIRequestLimit > 0 {
			effective := float64(base.APIRequests+cur.APIRequests) / float64(lim.APIRequestLimit)
			if effective >= threshold {
				return true
			}
		}
		if lim.EgressByteLimit > 0 {
			effective := float64(base.EgressBytes+cur.EgressBytes) / float64(lim.EgressByteLimit)
			if effective >= threshold {
				return true
			}
		}
		if lim.IngressByteLimit > 0 {
			effective := float64(base.IngressBytes+cur.IngressBytes) / float64(lim.IngressByteLimit)
			if effective >= threshold {
				return true
			}
		}
	}
	return false
}

// -------------------------------------------------------------------------
// FLUSH
// -------------------------------------------------------------------------

// currentPeriod returns the current month as "YYYY-MM" for usage aggregation.
func currentPeriod() string {
	return time.Now().UTC().Format("2006-01")
}

// FlushUsage reads and resets the counter backend, then writes the accumulated
// deltas to the database. Called periodically (every 30s). On DB error, deltas
// are added back to avoid data loss. Backends in the skip set have their
// counters discarded (used for drained backends whose DB records are gone).
func (u *UsageTracker) FlushUsage(ctx context.Context, store MetadataStore, skip map[string]bool) error {
	period := currentPeriod()
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
			slog.Error("Failed to flush usage deltas", "backend", name, "error", err)
			lastErr = err
		}
	}

	return lastErr
}
