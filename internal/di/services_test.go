// -------------------------------------------------------------------------------
// Background Service Tests
//
// Author: Alex Freidah
//
// The usage flush service, which runs on its own ticker rather than the shared
// tick runner: which steps a tick runs, and how often it ticks at the normal
// and the adaptive interval. Also holds the services fixture the other test
// files build services from. The tick runner itself is tested in its package.
// -------------------------------------------------------------------------------

package di

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/proxy/expiry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/proxy/reconcile"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// recordingFlusher is a usageFlushOps that appends each flush call to calls,
// shared with recordingFleet so a test sees the whole tick in order.
type recordingFlusher struct {
	cfg   *config.UsageFlushConfig
	redis bool
	err   error
	calls *[]string
}

func (r recordingFlusher) Config() *config.UsageFlushConfig { return r.cfg }
func (r recordingFlusher) RedisCounterConfigured() bool     { return r.redis }

func (r recordingFlusher) FlushUsage(_ context.Context) error {
	*r.calls = append(*r.calls, "FlushUsage")
	return r.err
}

func (r recordingFlusher) RefreshQuotaBaselines(_ context.Context) error {
	*r.calls = append(*r.calls, "RefreshQuotaBaselines")
	return r.err
}

// recordingFleet is a quotaMetricsRefresher that appends each refresh to calls.
type recordingFleet struct {
	err   error
	calls *[]string
}

func (r recordingFleet) RefreshFleetIfStale(_ context.Context) error {
	*r.calls = append(*r.calls, "RefreshFleetIfStale")
	return r.err
}

func (r recordingFleet) LoadFleetMetrics(_ context.Context) error {
	*r.calls = append(*r.calls, "LoadFleetMetrics")
	return r.err
}

func (r recordingFleet) LoadWorkerGauges(_ context.Context) error {
	*r.calls = append(*r.calls, "LoadWorkerGauges")
	return r.err
}

func (r recordingFleet) RefreshUsageBaselines(_ context.Context) error {
	*r.calls = append(*r.calls, "RefreshUsageBaselines")
	return r.err
}

// recordingStates records each backend state refresh into the shared call log.
type recordingStates struct {
	err   error
	calls *[]string
}

func (r recordingStates) Refresh(_ context.Context) error {
	*r.calls = append(*r.calls, "RefreshBackendStates")
	return r.err
}

// noStates is a backend state refresher with nothing to refresh, for tests
// that do not look at the state cache.
type noStates struct{}

func (noStates) Refresh(_ context.Context) error { return nil }

// nearLimit reports a fixed answer to the adaptive interval's question.
type nearLimit bool

func (n nearLimit) NearLimit(_ float64) bool { return bool(n) }

// servicesFixture bundles the proxy stack with the workers that the
// background-service factories accept. Each worker is a separate dependency.
type servicesFixture struct {
	stack         *proxytest.Stack
	expirer       *expiry.Manager
	reconciler    *reconcile.Manager
	rebalancer    *worker.Rebalancer
	replicator    *worker.Replicator
	overRep       *worker.OverReplicationCleaner
	cleanupWorker *worker.CleanupWorker
	scrubber      *worker.Scrubber
}

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// newServicesFixture builds the stack and the workers the service
// constructors need. Mirrors what the per-worker DI providers do at
// runtime, just inline so the test stays free of the do.Injector.
func newServicesFixture(t *testing.T) *servicesFixture {
	t.Helper()
	mock := storetest.NewMockMetadataStore(gomock.NewController(t))
	storetest.Permissive(mock)
	st := proxytest.New(t, mock, &proxytest.StackOptions{
		Runtime: proxytest.NewRuntime(&proxytest.RuntimeOptions{
			Backends:        map[string]backend.ObjectBackend{},
			Order:           []string{},
			RoutingStrategy: config.RoutingPack,
			Metrics:         mock,
		}),
	})

	rt := st.Runtime
	coord := writepath.New(rt, mock)
	rb := worker.NewRebalancer(rt, coord, mock)
	rb.SetConfig(&config.RebalanceConfig{})

	rp := worker.NewReplicator(worker.ReplicatorDeps{Ops: rt, Placement: coord, Store: mock})
	rp.SetConfig(&config.ReplicationConfig{Factor: 1})

	or := worker.NewOverReplicationCleaner(rt, coord, mock)
	or.SetConfig(&config.ReplicationConfig{Factor: 1})

	cw := worker.NewCleanupWorker(worker.CleanupWorkerDeps{Ops: rt, Store: mock, Concurrency: 10, InstanceID: "test-instance", ClaimGracePeriod: 5 * time.Minute})

	sc := worker.NewScrubber(worker.ScrubberDeps{Ops: rt, Placement: coord, Store: mock})
	sc.SetConfig(&config.IntegrityConfig{})

	rec := reconcile.NewManager(&reconcile.Deps{
		Backends: rt, Stores: mock, Usage: rt.Acct(), Quota: rt.Quota(),
	})
	exp := expiry.New(mock, st.Objects, nil)
	exp.SetConfig(&config.LifecycleConfig{})
	st.IntegrityCfg.Store(&config.IntegrityConfig{})
	return &servicesFixture{
		stack:         st,
		expirer:       exp,
		reconciler:    rec,
		rebalancer:    rb,
		replicator:    rp,
		overRep:       or,
		cleanupWorker: cw,
		scrubber:      sc,
	}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestUsageFlushService_FlushTick pins which steps a tick runs and in what
// order. Every instance flushes its counters, since the service holds no lock
// to lose. With Redis the instance also loads the published fleet snapshot;
// it never computes one. Store errors never cut the tick short.
func TestUsageFlushService_FlushTick(t *testing.T) {
	t.Parallel()
	storeErr := errors.New("store down")
	single := []string{"RefreshQuotaBaselines", "FlushUsage", "RefreshUsageBaselines", "RefreshBackendStates"}
	shared := []string{"RefreshQuotaBaselines", "FlushUsage", "LoadFleetMetrics", "LoadWorkerGauges", "RefreshUsageBaselines", "RefreshBackendStates"}

	tests := []struct {
		name  string
		redis bool
		err   error
		want  []string
	}{
		{"redis loads fleet state", true, nil, shared},
		{"no redis skips fleet state", false, nil, single},
		{"errors do not stop the tick", true, storeErr, shared},
		{"db unavailable is quiet", false, core.ErrDBUnavailable, single},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var calls []string
			svc := NewUsageFlushService(&UsageFlushDeps{
				Flusher: recordingFlusher{redis: tt.redis, err: tt.err, calls: &calls},
				Tracker: nearLimit(false),
				Fleet:   recordingFleet{err: tt.err, calls: &calls},
				States:  recordingStates{err: tt.err, calls: &calls},
			}).(*usageFlushService)

			svc.flushTick(t.Context())

			if !slices.Equal(calls, tt.want) {
				t.Errorf("calls = %v, want %v", calls, tt.want)
			}
		})
	}
}

// TestUsageFlushService_RunFlushesEachInterval drives Run on a fake clock. It
// flushes once per configured interval, and once a backend is near its limit
// with adaptive flushing on, the ticks after the first come at the fast
// interval.
func TestUsageFlushService_RunFlushesEachInterval(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *config.UsageFlushConfig
		near    nearLimit
		elapsed time.Duration
		want    int
	}{
		{"default interval with no config", nil, false, 95 * time.Second, 3},
		{"configured interval", &config.UsageFlushConfig{Interval: 10 * time.Second}, false, 45 * time.Second, 4},
		{"adaptive off ignores the limit", &config.UsageFlushConfig{Interval: 30 * time.Second, FastInterval: 5 * time.Second}, true, 52 * time.Second, 1},
		{"adaptive switches to the fast interval", &config.UsageFlushConfig{
			Interval: 30 * time.Second, FastInterval: 5 * time.Second, AdaptiveEnabled: true, AdaptiveThreshold: 0.8,
		}, true, 52 * time.Second, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				if got := flushesIn(t, tt.cfg, tt.near, tt.elapsed); got != tt.want {
					t.Errorf("flushes in %v = %d, want %d", tt.elapsed, got, tt.want)
				}
			})
		})
	}
}

// flushesIn runs the usage flush service for elapsed on the bubble's fake
// clock, stops it, and returns how many times it flushed.
func flushesIn(t *testing.T, cfg *config.UsageFlushConfig, near nearLimit, elapsed time.Duration) int {
	t.Helper()
	var calls []string
	svc := NewUsageFlushService(&UsageFlushDeps{
		Flusher: recordingFlusher{cfg: cfg, calls: &calls},
		Tracker: near,
		Fleet:   recordingFleet{calls: &calls},
		States:  noStates{},
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error)
	go func() { done <- svc.Run(ctx) }()

	synctest.Sleep(elapsed)
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned %v on cancel, want nil", err)
	}
	flushes := 0
	for _, c := range calls {
		if c == "FlushUsage" {
			flushes++
		}
	}
	return flushes
}
