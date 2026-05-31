// -------------------------------------------------------------------------------
// Usage Tracking Tests
//
// Author: Alex Freidah
//
// Tests for BackendManager usage limit enforcement. Validates API request limits,
// egress and ingress byte caps, near-limit detection thresholds, and monthly
// counter reset behavior.
// -------------------------------------------------------------------------------

package proxy

import (
	"context"
	"fmt"
	"regexp"
	"sync"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// flushDeltaCall records one FlushUsageDeltas invocation for assertion.
type flushDeltaCall struct {
	backendName  string
	period       string
	apiRequests  int64
	egressBytes  int64
	ingressBytes int64
}

// flushTracker captures every FlushUsageDeltas call into a slice and
// returns the configured error from the underlying store stub. Used by
// flush-path tests that want to assert what the system pushed to the
// metadata store.
type flushTracker struct {
	mu    sync.Mutex
	calls []flushDeltaCall
	err   error
}

// stubFlushUsage returns a DoAndReturn that records each call into ft.
func stubFlushUsage(ft *flushTracker) func(context.Context, string, string, int64, int64, int64) error {
	return func(_ context.Context, backendName, period string, apiRequests, egressBytes, ingressBytes int64) error {
		ft.mu.Lock()
		defer ft.mu.Unlock()
		ft.calls = append(ft.calls, flushDeltaCall{
			backendName:  backendName,
			period:       period,
			apiRequests:  apiRequests,
			egressBytes:  egressBytes,
			ingressBytes: ingressBytes,
		})
		return ft.err
	}
}

// newUsageManager creates a BackendManager with the given backend names and a
// configurable mock store.
func newUsageManager(t *testing.T, backendNames []string, store core.MetadataStore) *BackendManager {
	t.Helper()
	backends := make(map[string]backend.ObjectBackend, len(backendNames))
	for _, name := range backendNames {
		backends[name] = newMockBackend()
	}
	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        backends,
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           backendNames,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr, store)
	_ = workers
	return mgr
}

// newUsageManagerWithLimits constructs a new usage manager with limits.
func newUsageManagerWithLimits(t *testing.T, backendNames []string, store core.MetadataStore, limits map[string]core.UsageLimits) *BackendManager {
	t.Helper()
	backends := make(map[string]backend.ObjectBackend, len(backendNames))
	for _, name := range backendNames {
		backends[name] = newMockBackend()
	}
	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        backends,
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           backendNames,
		UsageLimits:     limits,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr, store)
	_ = workers
	return mgr
}

// usageStoreWithFlush returns a permissive MockMetadataStore plus a
// flushTracker the test can read after exercising the manager. The
// FlushUsageDeltas expectation lives on the tracker so the err field is
// thread-safely shared.
func usageStoreWithFlush(t *testing.T) (*storetest.MockMetadataStore, *flushTracker) {
	t.Helper()
	ft := &flushTracker{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().FlushUsageDeltas(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubFlushUsage(ft)).AnyTimes()
	storetest.Permissive(store)
	return store, ft
}

// --- recordUsage tests ---

// TestRecordUsage_IncrementsCounters verifies the record usage increments
// counters contract.
func TestRecordUsage_IncrementsCounters(t *testing.T) {
	t.Parallel()
	mgr := newUsageManager(t, []string{"b1"}, newPermissiveMock(t))

	mgr.Usage().Record("b1", 3, 1024, 2048)

	c := mgr.Usage().Backend().LoadAll("b1")
	if c.APIRequests != 3 {
		t.Errorf("apiRequests = %d, want 3", c.APIRequests)
	}
	if c.EgressBytes != 1024 {
		t.Errorf("egressBytes = %d, want 1024", c.EgressBytes)
	}
	if c.IngressBytes != 2048 {
		t.Errorf("ingressBytes = %d, want 2048", c.IngressBytes)
	}
}

// TestRecordUsage_Accumulates verifies multiple Record calls accumulate.
func TestRecordUsage_Accumulates(t *testing.T) {
	t.Parallel()
	mgr := newUsageManager(t, []string{"b1"}, newPermissiveMock(t))

	mgr.Usage().Record("b1", 1, 100, 200)
	mgr.Usage().Record("b1", 2, 300, 400)

	c := mgr.Usage().Backend().LoadAll("b1")
	if c.APIRequests != 3 {
		t.Errorf("apiRequests = %d, want 3", c.APIRequests)
	}
	if c.EgressBytes != 400 {
		t.Errorf("egressBytes = %d, want 400", c.EgressBytes)
	}
	if c.IngressBytes != 600 {
		t.Errorf("ingressBytes = %d, want 600", c.IngressBytes)
	}
}

// TestRecordUsage_UnknownBackendNoOp verifies the unknown-backend no-op.
func TestRecordUsage_UnknownBackendNoOp(t *testing.T) {
	t.Parallel()
	mgr := newUsageManager(t, []string{"b1"}, newPermissiveMock(t))

	mgr.Usage().Record("unknown", 1, 1, 1)

	if got := mgr.Usage().Backend().Load("b1", counter.FieldAPIRequests); got != 0 {
		t.Errorf("apiRequests = %d, want 0", got)
	}
}

// TestRecordUsage_ZeroValuesSkipped verifies a zero-value record is a
// no-op.
func TestRecordUsage_ZeroValuesSkipped(t *testing.T) {
	t.Parallel()
	mgr := newUsageManager(t, []string{"b1"}, newPermissiveMock(t))

	mgr.Usage().Record("b1", 0, 0, 0)

	if got := mgr.Usage().Backend().Load("b1", counter.FieldAPIRequests); got != 0 {
		t.Errorf("apiRequests = %d, want 0", got)
	}
}

// TestRecordUsage_MultipleBackends verifies records to distinct backends
// don't collide.
func TestRecordUsage_MultipleBackends(t *testing.T) {
	t.Parallel()
	mgr := newUsageManager(t, []string{"b1", "b2"}, newPermissiveMock(t))

	mgr.Usage().Record("b1", 1, 100, 0)
	mgr.Usage().Record("b2", 2, 0, 200)

	if got := mgr.Usage().Backend().Load("b1", counter.FieldAPIRequests); got != 1 {
		t.Errorf("b1 apiRequests = %d, want 1", got)
	}
	if got := mgr.Usage().Backend().Load("b2", counter.FieldIngressBytes); got != 200 {
		t.Errorf("b2 ingressBytes = %d, want 200", got)
	}
}

// TestRecordUsage_PublicMethod verifies the public RecordUsage forwards
// to the tracker.
func TestRecordUsage_PublicMethod(t *testing.T) {
	t.Parallel()
	mgr := newUsageManager(t, []string{"b1"}, newPermissiveMock(t))

	mgr.RecordUsage("b1", 5, 1024, 2048)

	c := mgr.Usage().Backend().LoadAll("b1")
	if c.APIRequests != 5 {
		t.Errorf("apiRequests = %d, want 5", c.APIRequests)
	}
	if c.EgressBytes != 1024 {
		t.Errorf("egressBytes = %d, want 1024", c.EgressBytes)
	}
	if c.IngressBytes != 2048 {
		t.Errorf("ingressBytes = %d, want 2048", c.IngressBytes)
	}
}

// --- FlushUsage tests ---

// TestFlushUsage_WritesToStore confirms the manager writes the buffered
// usage to the store and resets counters.
func TestFlushUsage_WritesToStore(t *testing.T) {
	t.Parallel()
	store, ft := usageStoreWithFlush(t)
	mgr := newUsageManager(t, []string{"b1"}, store)

	mgr.Usage().Record("b1", 5, 1024, 2048)

	if err := mgr.FlushUsage(context.Background()); err != nil {
		t.Fatalf("FlushUsage() error = %v", err)
	}

	c := mgr.Usage().Backend().LoadAll("b1")
	if c.APIRequests != 0 || c.EgressBytes != 0 || c.IngressBytes != 0 {
		t.Errorf("counters not reset after flush: %+v", c)
	}

	ft.mu.Lock()
	defer ft.mu.Unlock()
	if len(ft.calls) != 1 {
		t.Fatalf("flushDeltaCalls = %d, want 1", len(ft.calls))
	}
	call := ft.calls[0]
	if call.backendName != "b1" || call.apiRequests != 5 || call.egressBytes != 1024 || call.ingressBytes != 2048 {
		t.Errorf("flush call = %+v, want b1/5/1024/2048", call)
	}
}

// TestFlushUsage_SkipsZeroDeltas confirms backends with no recorded
// deltas are skipped.
func TestFlushUsage_SkipsZeroDeltas(t *testing.T) {
	t.Parallel()
	store, ft := usageStoreWithFlush(t)
	mgr := newUsageManager(t, []string{"b1", "b2"}, store)

	mgr.Usage().Record("b1", 1, 0, 0)

	if err := mgr.FlushUsage(context.Background()); err != nil {
		t.Fatalf("FlushUsage() error = %v", err)
	}

	ft.mu.Lock()
	defer ft.mu.Unlock()
	if len(ft.calls) != 1 {
		t.Fatalf("flushDeltaCalls = %d, want 1 (b2 should be skipped)", len(ft.calls))
	}
	if ft.calls[0].backendName != "b1" {
		t.Errorf("flushed backend = %s, want b1", ft.calls[0].backendName)
	}
}

// TestFlushUsage_RestoresCountersOnError pins the rollback path: a
// FlushUsageDeltas failure must put the in-memory counters back where
// they were so a later flush can retry.
func TestFlushUsage_RestoresCountersOnError(t *testing.T) {
	t.Parallel()
	store, ft := usageStoreWithFlush(t)
	ft.err = fmt.Errorf("db down")
	mgr := newUsageManager(t, []string{"b1"}, store)

	mgr.Usage().Record("b1", 10, 500, 300)

	if err := mgr.FlushUsage(context.Background()); err == nil {
		t.Fatal("FlushUsage() should return error")
	}

	c := mgr.Usage().Backend().LoadAll("b1")
	if c.APIRequests != 10 {
		t.Errorf("apiRequests after failed flush = %d, want 10 (restored)", c.APIRequests)
	}
	if c.EgressBytes != 500 {
		t.Errorf("egressBytes after failed flush = %d, want 500 (restored)", c.EgressBytes)
	}
	if c.IngressBytes != 300 {
		t.Errorf("ingressBytes after failed flush = %d, want 300 (restored)", c.IngressBytes)
	}
}

// TestFlushUsage_NoDataNoCall confirms an idle flush issues no store
// calls.
func TestFlushUsage_NoDataNoCall(t *testing.T) {
	t.Parallel()
	store, ft := usageStoreWithFlush(t)
	mgr := newUsageManager(t, []string{"b1"}, store)

	if err := mgr.FlushUsage(context.Background()); err != nil {
		t.Fatalf("FlushUsage() error = %v", err)
	}

	ft.mu.Lock()
	defer ft.mu.Unlock()
	if len(ft.calls) != 0 {
		t.Errorf("flushDeltaCalls = %d, want 0", len(ft.calls))
	}
}

// TestFlushUsage_SkipsDrainedBackend confirms backends marked
// drain-completed are skipped during flush.
func TestFlushUsage_SkipsDrainedBackend(t *testing.T) {
	t.Parallel()
	store, ft := usageStoreWithFlush(t)
	mgr := newUsageManager(t, []string{"b1", "b2"}, store)

	mgr.Usage().Record("b1", 5, 100, 200)
	mgr.Usage().Record("b2", 3, 50, 75)

	mgr.drainManager.SeedCompletedForTest("b2")

	if err := mgr.FlushUsage(context.Background()); err != nil {
		t.Fatalf("FlushUsage() error = %v", err)
	}

	ft.mu.Lock()
	defer ft.mu.Unlock()
	if len(ft.calls) != 1 {
		t.Fatalf("flushDeltaCalls = %d, want 1 (b2 should be skipped)", len(ft.calls))
	}
	if ft.calls[0].backendName != "b1" {
		t.Errorf("flushed backend = %s, want b1", ft.calls[0].backendName)
	}

	if got := mgr.Usage().Backend().Load("b2", counter.FieldAPIRequests); got != 0 {
		t.Errorf("b2 apiRequests = %d, want 0 (discarded)", got)
	}
}

// --- withinUsageLimits tests ---

// TestWithinUsageLimits_NoLimits confirms the no-limits short-circuit.
func TestWithinUsageLimits_NoLimits(t *testing.T) {
	t.Parallel()
	mgr := newUsageManager(t, []string{"b1"}, newPermissiveMock(t))

	if !mgr.Usage().WithinLimits("b1", 1000, 1000, 1000) {
		t.Error("no limits configured, should always return true")
	}
}

// TestWithinUsageLimits_ApiExceeded confirms the API-limit branch.
func TestWithinUsageLimits_ApiExceeded(t *testing.T) {
	t.Parallel()
	limits := map[string]core.UsageLimits{
		"b1": {APIRequestLimit: 100},
	}
	mgr := newUsageManagerWithLimits(t, []string{"b1"}, newPermissiveMock(t), limits)

	mgr.Usage().SetBaseline("b1", core.UsageStat{APIRequests: 100})

	if mgr.Usage().WithinLimits("b1", 1, 0, 0) {
		t.Error("should exceed API request limit")
	}
}

// TestWithinUsageLimits_EgressExceeded confirms the egress-limit branch.
func TestWithinUsageLimits_EgressExceeded(t *testing.T) {
	t.Parallel()
	limits := map[string]core.UsageLimits{
		"b1": {EgressByteLimit: 1000},
	}
	mgr := newUsageManagerWithLimits(t, []string{"b1"}, newPermissiveMock(t), limits)

	mgr.Usage().SetBaseline("b1", core.UsageStat{EgressBytes: 500})
	mgr.Usage().Backend().Add("b1", counter.FieldEgressBytes, 400)

	if mgr.Usage().WithinLimits("b1", 0, 200, 0) {
		t.Error("should exceed egress byte limit")
	}
}

// TestWithinUsageLimits_UnlimitedDimension confirms a zero-valued limit
// is treated as unlimited.
func TestWithinUsageLimits_UnlimitedDimension(t *testing.T) {
	t.Parallel()
	limits := map[string]core.UsageLimits{
		"b1": {APIRequestLimit: 100, EgressByteLimit: 0, IngressByteLimit: 0},
	}
	mgr := newUsageManagerWithLimits(t, []string{"b1"}, newPermissiveMock(t), limits)

	if !mgr.Usage().WithinLimits("b1", 1, 999999, 999999) {
		t.Error("zero limit means unlimited, should not be checked")
	}
}

// TestBackendsWithinLimits_FiltersCorrectly pins the per-backend filter
// behaviour.
func TestBackendsWithinLimits_FiltersCorrectly(t *testing.T) {
	t.Parallel()
	limits := map[string]core.UsageLimits{
		"b1": {APIRequestLimit: 10},
		"b2": {APIRequestLimit: 100},
	}
	mgr := newUsageManagerWithLimits(t, []string{"b1", "b2"}, newPermissiveMock(t), limits)

	mgr.Usage().SetBaseline("b1", core.UsageStat{APIRequests: 10})

	eligible := mgr.Usage().BackendsWithinLimits(mgr.BackendOrder(), 1, 0, 0)
	if len(eligible) != 1 {
		t.Fatalf("eligible = %v, want [b2]", eligible)
	}
	if eligible[0] != "b2" {
		t.Errorf("eligible[0] = %q, want %q", eligible[0], "b2")
	}
}

// --- currentPeriod tests ---

// TestCurrentPeriod_Format pins the YYYY-MM format.
func TestCurrentPeriod_Format(t *testing.T) {
	t.Parallel()
	period := counter.CurrentPeriod()

	matched, err := regexp.MatchString(`^\d{4}-\d{2}$`, period)
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Errorf("counter.CurrentPeriod() = %q, want YYYY-MM format", period)
	}

	expected := time.Now().UTC().Format("2006-01")
	if period != expected {
		t.Errorf("counter.CurrentPeriod() = %q, want %q", period, expected)
	}
}
