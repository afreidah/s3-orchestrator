// -------------------------------------------------------------------------------
// Usage Flush Baseline Tests
//
// Author: Alex Freidah
//
// Covers the usage baseline refresh on an instance that loses the usage-flush
// advisory lock. With Redis counters several instances share the counters but
// each holds its own baseline, and limit checks compare against that baseline.
// -------------------------------------------------------------------------------

package di

import (
	"context"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// sharedCounterFlusher is a usageFlushOps that reports Redis counters as
// configured, so the flush tick takes the multi-instance path.
type sharedCounterFlusher struct{}

func (sharedCounterFlusher) Config() *config.UsageFlushConfig   { return nil }
func (sharedCounterFlusher) RedisCounterConfigured() bool       { return true }
func (sharedCounterFlusher) FlushUsage(_ context.Context) error { return nil }
func (sharedCounterFlusher) FlushQuota(_ context.Context) error { return nil }

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

// TestUsageFlushService_LockLoserRefreshesBaseline asserts that an instance
// which loses the usage-flush lock still reloads its usage baseline from the
// store. The store reports 900 of a 1000-byte egress budget spent, so once the
// baseline is loaded a 200-byte read must be refused. An instance that skips
// the refresh keeps an empty baseline and admits it.
func TestUsageFlushService_LockLoserRefreshesBaseline(t *testing.T) {
	t.Parallel()
	const name = "b1"

	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{name: {}}, nil).AnyTimes()
	store.EXPECT().GetUsageForPeriod(gomock.Any(), gomock.Any()).
		Return(map[string]core.UsageStat{name: {EgressBytes: 900}}, nil).AnyTimes()
	storetest.Permissive(store)

	rt := proxytest.NewRuntime(&proxytest.RuntimeOptions{
		Backends:        map[string]backend.ObjectBackend{},
		Order:           []string{name},
		RoutingStrategy: config.RoutingPack,
		UsageLimits:     map[string]core.UsageLimits{name: {EgressByteLimit: 1000}},
		Metrics:         store,
	})
	read := []s3op.Operation{s3op.GetObject}
	if !rt.Usage().WithinLimits(name, read, 200, 0) {
		t.Fatal("read refused before any baseline was loaded")
	}

	svc := NewUsageFlushService(&UsageFlushDeps{
		Flusher: sharedCounterFlusher{},
		Tracker: rt.Usage(),
		Fleet:   rt,
		Locker:  fakeLocker{},
	}).(*usageFlushService)
	svc.flushTick(context.Background())

	if rt.Usage().WithinLimits(name, read, 200, 0) {
		t.Error("lock loser admitted a read past its egress limit: usage baseline was not refreshed")
	}
}
