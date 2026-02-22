package storage

import (
	"context"
	"fmt"
	"regexp"
	"testing"
	"time"
)

// --- Test helpers ---

// newUsageManager creates a BackendManager with the given backend names and a
// configurable mock store. The mock's flushUsageErr field controls whether
// FlushUsageDeltas returns an error.
func newUsageManager(backendNames []string, store *mockStore) *BackendManager {
	backends := make(map[string]ObjectBackend, len(backendNames))
	for _, name := range backendNames {
		backends[name] = newMockBackend()
	}
	return NewBackendManager(BackendManagerConfig{
		Backends:        backends,
		Store:           store,
		Order:           backendNames,
		RoutingStrategy: "pack",
	})
}

func newUsageManagerWithLimits(backendNames []string, store *mockStore, limits map[string]UsageLimits) *BackendManager {
	backends := make(map[string]ObjectBackend, len(backendNames))
	for _, name := range backendNames {
		backends[name] = newMockBackend()
	}
	return NewBackendManager(BackendManagerConfig{
		Backends:        backends,
		Store:           store,
		Order:           backendNames,
		UsageLimits:     limits,
		RoutingStrategy: "pack",
	})
}

// --- recordUsage tests ---

func TestRecordUsage_IncrementsCounters(t *testing.T) {
	mgr := newUsageManager([]string{"b1"}, &mockStore{})

	mgr.recordUsage("b1", 3, 1024, 2048)

	c := mgr.usage["b1"]
	if got := c.apiRequests.Load(); got != 3 {
		t.Errorf("apiRequests = %d, want 3", got)
	}
	if got := c.egressBytes.Load(); got != 1024 {
		t.Errorf("egressBytes = %d, want 1024", got)
	}
	if got := c.ingressBytes.Load(); got != 2048 {
		t.Errorf("ingressBytes = %d, want 2048", got)
	}
}

func TestRecordUsage_Accumulates(t *testing.T) {
	mgr := newUsageManager([]string{"b1"}, &mockStore{})

	mgr.recordUsage("b1", 1, 100, 200)
	mgr.recordUsage("b1", 2, 300, 400)

	c := mgr.usage["b1"]
	if got := c.apiRequests.Load(); got != 3 {
		t.Errorf("apiRequests = %d, want 3", got)
	}
	if got := c.egressBytes.Load(); got != 400 {
		t.Errorf("egressBytes = %d, want 400", got)
	}
	if got := c.ingressBytes.Load(); got != 600 {
		t.Errorf("ingressBytes = %d, want 600", got)
	}
}

func TestRecordUsage_UnknownBackendNoOp(t *testing.T) {
	mgr := newUsageManager([]string{"b1"}, &mockStore{})

	// Should not panic for unknown backend
	mgr.recordUsage("unknown", 1, 1, 1)

	c := mgr.usage["b1"]
	if got := c.apiRequests.Load(); got != 0 {
		t.Errorf("apiRequests = %d, want 0", got)
	}
}

func TestRecordUsage_ZeroValuesSkipped(t *testing.T) {
	mgr := newUsageManager([]string{"b1"}, &mockStore{})

	mgr.recordUsage("b1", 0, 0, 0)

	c := mgr.usage["b1"]
	if got := c.apiRequests.Load(); got != 0 {
		t.Errorf("apiRequests = %d, want 0", got)
	}
}

func TestRecordUsage_MultipleBackends(t *testing.T) {
	mgr := newUsageManager([]string{"b1", "b2"}, &mockStore{})

	mgr.recordUsage("b1", 1, 100, 0)
	mgr.recordUsage("b2", 2, 0, 200)

	if got := mgr.usage["b1"].apiRequests.Load(); got != 1 {
		t.Errorf("b1 apiRequests = %d, want 1", got)
	}
	if got := mgr.usage["b2"].ingressBytes.Load(); got != 200 {
		t.Errorf("b2 ingressBytes = %d, want 200", got)
	}
}

// --- FlushUsage tests ---

func TestFlushUsage_WritesToStore(t *testing.T) {
	ms := &mockStore{}
	mgr := newUsageManager([]string{"b1"}, ms)

	mgr.recordUsage("b1", 5, 1024, 2048)

	if err := mgr.FlushUsage(context.Background()); err != nil {
		t.Fatalf("FlushUsage() error = %v", err)
	}

	// Counters should be reset
	c := mgr.usage["b1"]
	if got := c.apiRequests.Load(); got != 0 {
		t.Errorf("apiRequests after flush = %d, want 0", got)
	}
	if got := c.egressBytes.Load(); got != 0 {
		t.Errorf("egressBytes after flush = %d, want 0", got)
	}
	if got := c.ingressBytes.Load(); got != 0 {
		t.Errorf("ingressBytes after flush = %d, want 0", got)
	}

	// Mock should have received the call
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if len(ms.flushUsageCalls) != 1 {
		t.Fatalf("flushUsageCalls = %d, want 1", len(ms.flushUsageCalls))
	}
	call := ms.flushUsageCalls[0]
	if call.backendName != "b1" || call.apiRequests != 5 || call.egressBytes != 1024 || call.ingressBytes != 2048 {
		t.Errorf("flush call = %+v, want b1/5/1024/2048", call)
	}
}

func TestFlushUsage_SkipsZeroDeltas(t *testing.T) {
	ms := &mockStore{}
	mgr := newUsageManager([]string{"b1", "b2"}, ms)

	// Only increment b1
	mgr.recordUsage("b1", 1, 0, 0)

	if err := mgr.FlushUsage(context.Background()); err != nil {
		t.Fatalf("FlushUsage() error = %v", err)
	}

	ms.mu.Lock()
	defer ms.mu.Unlock()
	if len(ms.flushUsageCalls) != 1 {
		t.Fatalf("flushUsageCalls = %d, want 1 (b2 should be skipped)", len(ms.flushUsageCalls))
	}
	if ms.flushUsageCalls[0].backendName != "b1" {
		t.Errorf("flushed backend = %s, want b1", ms.flushUsageCalls[0].backendName)
	}
}

func TestFlushUsage_RestoresCountersOnError(t *testing.T) {
	ms := &mockStore{
		flushUsageErr: fmt.Errorf("db down"),
	}
	mgr := newUsageManager([]string{"b1"}, ms)

	mgr.recordUsage("b1", 10, 500, 300)

	err := mgr.FlushUsage(context.Background())
	if err == nil {
		t.Fatal("FlushUsage() should return error")
	}

	// Counters should be restored
	c := mgr.usage["b1"]
	if got := c.apiRequests.Load(); got != 10 {
		t.Errorf("apiRequests after failed flush = %d, want 10 (restored)", got)
	}
	if got := c.egressBytes.Load(); got != 500 {
		t.Errorf("egressBytes after failed flush = %d, want 500 (restored)", got)
	}
	if got := c.ingressBytes.Load(); got != 300 {
		t.Errorf("ingressBytes after failed flush = %d, want 300 (restored)", got)
	}
}

func TestFlushUsage_NoDataNoCall(t *testing.T) {
	ms := &mockStore{}
	mgr := newUsageManager([]string{"b1"}, ms)

	if err := mgr.FlushUsage(context.Background()); err != nil {
		t.Fatalf("FlushUsage() error = %v", err)
	}

	ms.mu.Lock()
	defer ms.mu.Unlock()
	if len(ms.flushUsageCalls) != 0 {
		t.Errorf("flushUsageCalls = %d, want 0", len(ms.flushUsageCalls))
	}
}

// --- withinUsageLimits tests ---

func TestWithinUsageLimits_NoLimits(t *testing.T) {
	mgr := newUsageManager([]string{"b1"}, &mockStore{})

	if !mgr.withinUsageLimits("b1", 1000, 1000, 1000) {
		t.Error("no limits configured, should always return true")
	}
}

func TestWithinUsageLimits_ApiExceeded(t *testing.T) {
	limits := map[string]UsageLimits{
		"b1": {ApiRequestLimit: 100},
	}
	mgr := newUsageManagerWithLimits([]string{"b1"}, &mockStore{}, limits)

	// Set baseline to exactly the limit
	mgr.usageBaselineMu.Lock()
	mgr.usageBaseline["b1"] = UsageStat{ApiRequests: 100}
	mgr.usageBaselineMu.Unlock()

	if mgr.withinUsageLimits("b1", 1, 0, 0) {
		t.Error("should exceed API request limit")
	}
}

func TestWithinUsageLimits_EgressExceeded(t *testing.T) {
	limits := map[string]UsageLimits{
		"b1": {EgressByteLimit: 1000},
	}
	mgr := newUsageManagerWithLimits([]string{"b1"}, &mockStore{}, limits)

	// Baseline: 500, unflushed: add 400, proposed: 200 → 1100 > 1000
	mgr.usageBaselineMu.Lock()
	mgr.usageBaseline["b1"] = UsageStat{EgressBytes: 500}
	mgr.usageBaselineMu.Unlock()
	mgr.usage["b1"].egressBytes.Store(400)

	if mgr.withinUsageLimits("b1", 0, 200, 0) {
		t.Error("should exceed egress byte limit")
	}
}

func TestWithinUsageLimits_UnlimitedDimension(t *testing.T) {
	limits := map[string]UsageLimits{
		"b1": {ApiRequestLimit: 100, EgressByteLimit: 0, IngressByteLimit: 0},
	}
	mgr := newUsageManagerWithLimits([]string{"b1"}, &mockStore{}, limits)

	// API within limit, egress/ingress unlimited (0)
	if !mgr.withinUsageLimits("b1", 1, 999999, 999999) {
		t.Error("zero limit means unlimited, should not be checked")
	}
}

func TestBackendsWithinLimits_FiltersCorrectly(t *testing.T) {
	limits := map[string]UsageLimits{
		"b1": {ApiRequestLimit: 10},
		"b2": {ApiRequestLimit: 100},
	}
	mgr := newUsageManagerWithLimits([]string{"b1", "b2"}, &mockStore{}, limits)

	// Push b1 over limit
	mgr.usageBaselineMu.Lock()
	mgr.usageBaseline["b1"] = UsageStat{ApiRequests: 10}
	mgr.usageBaselineMu.Unlock()

	eligible := mgr.backendsWithinLimits(1, 0, 0)
	if len(eligible) != 1 {
		t.Fatalf("eligible = %v, want [b2]", eligible)
	}
	if eligible[0] != "b2" {
		t.Errorf("eligible[0] = %q, want %q", eligible[0], "b2")
	}
}

// --- currentPeriod tests ---

func TestCurrentPeriod_Format(t *testing.T) {
	period := currentPeriod()

	matched, err := regexp.MatchString(`^\d{4}-\d{2}$`, period)
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Errorf("currentPeriod() = %q, want YYYY-MM format", period)
	}

	// Should match the current month
	expected := time.Now().UTC().Format("2006-01")
	if period != expected {
		t.Errorf("currentPeriod() = %q, want %q", period, expected)
	}
}
