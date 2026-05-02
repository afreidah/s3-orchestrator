// -------------------------------------------------------------------------------
// CB Decorator — DashboardStore
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package store

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// cbDashboardStore wraps a DashboardStore with circuit-breaker protection.
type cbDashboardStore struct {
	inner core.DashboardStore
	cb    *breaker.CircuitBreaker
}

// NewCBDashboardStore returns a CB-protected view typed as DashboardStore.
func NewCBDashboardStore(inner core.DashboardStore, cb *breaker.CircuitBreaker) core.DashboardStore {
	return &cbDashboardStore{inner: inner, cb: cb}
}

// GetQuotaStats forwards to the inner store under the breaker.
func (c *cbDashboardStore) GetQuotaStats(ctx context.Context) (map[string]core.QuotaStat, error) {
	return breaker.CBCall(c.cb, func() (map[string]core.QuotaStat, error) { return c.inner.GetQuotaStats(ctx) })
}

// GetObjectCounts forwards to the inner store under the breaker.
func (c *cbDashboardStore) GetObjectCounts(ctx context.Context) (map[string]int64, error) {
	return breaker.CBCall(c.cb, func() (map[string]int64, error) { return c.inner.GetObjectCounts(ctx) })
}

// GetActiveMultipartCounts forwards to the inner store under the breaker.
func (c *cbDashboardStore) GetActiveMultipartCounts(ctx context.Context) (map[string]int64, error) {
	return breaker.CBCall(c.cb, func() (map[string]int64, error) { return c.inner.GetActiveMultipartCounts(ctx) })
}

// GetUsageForPeriod forwards to the inner store under the breaker.
func (c *cbDashboardStore) GetUsageForPeriod(ctx context.Context, period string) (map[string]core.UsageStat, error) {
	return breaker.CBCall(c.cb, func() (map[string]core.UsageStat, error) { return c.inner.GetUsageForPeriod(ctx, period) })
}

// ListDirectoryChildren forwards to the inner store under the breaker.
func (c *cbDashboardStore) ListDirectoryChildren(ctx context.Context, prefix, startAfter string, maxKeys int) (*core.DirectoryListResult, error) {
	return breaker.CBCall(c.cb, func() (*core.DirectoryListResult, error) {
		return c.inner.ListDirectoryChildren(ctx, prefix, startAfter, maxKeys)
	})
}
