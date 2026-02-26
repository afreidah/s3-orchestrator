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
	"sync/atomic"
	"time"
)

// usageCounters holds atomic counters for a single backend's usage deltas.
// Incremented on the hot path (each request) and periodically flushed to the
// database.
type usageCounters struct {
	apiRequests  atomic.Int64
	egressBytes  atomic.Int64
	ingressBytes atomic.Int64
}

// UsageTracker tracks per-backend usage counters, enforces monthly usage limits,
// and flushes accumulated deltas to the database.
type UsageTracker struct {
	counters   map[string]*usageCounters
	limits     map[string]UsageLimits
	limitsMu   sync.RWMutex
	baseline   map[string]UsageStat
	baselineMu sync.RWMutex
}

// NewUsageTracker creates a usage tracker for the given backend names and limits.
func NewUsageTracker(backendNames []string, limits map[string]UsageLimits) *UsageTracker {
	counters := make(map[string]*usageCounters, len(backendNames))
	for _, name := range backendNames {
		counters[name] = &usageCounters{}
	}
	if limits == nil {
		limits = make(map[string]UsageLimits)
	}
	return &UsageTracker{
		counters: counters,
		limits:   limits,
		baseline: make(map[string]UsageStat),
	}
}

// Record increments the in-memory usage counters for a backend.
func (u *UsageTracker) Record(backendName string, apiCalls, egress, ingress int64) {
	c, ok := u.counters[backendName]
	if !ok {
		return
	}
	if apiCalls > 0 {
		c.apiRequests.Add(apiCalls)
	}
	if egress > 0 {
		c.egressBytes.Add(egress)
	}
	if ingress > 0 {
		c.ingressBytes.Add(ingress)
	}
}

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

	c := u.counters[backendName]
	if c == nil {
		return true
	}

	if lim.APIRequestLimit > 0 {
		effective := base.APIRequests + c.apiRequests.Load() + apiCalls
		if effective > lim.APIRequestLimit {
			return false
		}
	}
	if lim.EgressByteLimit > 0 {
		effective := base.EgressBytes + c.egressBytes.Load() + egress
		if effective > lim.EgressByteLimit {
			return false
		}
	}
	if lim.IngressByteLimit > 0 {
		effective := base.IngressBytes + c.ingressBytes.Load() + ingress
		if effective > lim.IngressByteLimit {
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

// UpdateLimits replaces the per-backend usage limits. Safe to call concurrently
// with request handling.
func (u *UsageTracker) UpdateLimits(limits map[string]UsageLimits) {
	u.limitsMu.Lock()
	defer u.limitsMu.Unlock()
	u.limits = limits
}

// GetLimits returns the current per-backend usage limits.
func (u *UsageTracker) GetLimits() map[string]UsageLimits {
	u.limitsMu.RLock()
	defer u.limitsMu.RUnlock()
	return u.limits
}

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
		c := u.counters[name]
		if c == nil {
			continue
		}

		if lim.APIRequestLimit > 0 {
			effective := float64(base.APIRequests+c.apiRequests.Load()) / float64(lim.APIRequestLimit)
			if effective >= threshold {
				return true
			}
		}
		if lim.EgressByteLimit > 0 {
			effective := float64(base.EgressBytes+c.egressBytes.Load()) / float64(lim.EgressByteLimit)
			if effective >= threshold {
				return true
			}
		}
		if lim.IngressByteLimit > 0 {
			effective := float64(base.IngressBytes+c.ingressBytes.Load()) / float64(lim.IngressByteLimit)
			if effective >= threshold {
				return true
			}
		}
	}
	return false
}

// currentPeriod returns the current month as "YYYY-MM" for usage aggregation.
func currentPeriod() string {
	return time.Now().UTC().Format("2006-01")
}

// FlushUsage reads and resets the in-memory atomic counters, then writes the
// accumulated deltas to the database. Called periodically (every 30s). On DB
// error, deltas are added back to avoid data loss.
func (u *UsageTracker) FlushUsage(ctx context.Context, store MetadataStore) error {
	period := currentPeriod()
	var lastErr error

	for name, counters := range u.counters {
		apiReqs := counters.apiRequests.Swap(0)
		egress := counters.egressBytes.Swap(0)
		ingress := counters.ingressBytes.Swap(0)

		if apiReqs == 0 && egress == 0 && ingress == 0 {
			continue
		}

		if err := store.FlushUsageDeltas(ctx, name, period, apiReqs, egress, ingress); err != nil {
			// Restore deltas so they aren't lost
			counters.apiRequests.Add(apiReqs)
			counters.egressBytes.Add(egress)
			counters.ingressBytes.Add(ingress)
			slog.Error("Failed to flush usage deltas", "backend", name, "error", err)
			lastErr = err
		}
	}

	return lastErr
}
