// -------------------------------------------------------------------------------
// Backend Core Tests - Circuit Breaker Filtering
//
// Author: Alex Freidah
//
// Unit tests for excludeUnhealthy which filters circuit-broken backends from
// the eligible list during write routing.
// -------------------------------------------------------------------------------

package proxy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// TestExcludeUnhealthy_FiltersOpenCircuitBreaker verifies the exclude unhealthy filters open circuit breaker contract.
// Asserts that expected 1 eligible backend, got.
func TestExcludeUnhealthy_FiltersOpenCircuitBreaker(t *testing.T) {
	t.Parallel()
	healthy := backend.NewCircuitBreakerBackend(newMockBackend(), "healthy", 3, 15*time.Second)

	failingMock := newMockBackend()
	failingMock.putErr = errors.New("backend down")
	unhealthy := backend.NewCircuitBreakerBackend(failingMock, "unhealthy", 1, 15*time.Second)

	// Trip the unhealthy backend's circuit breaker
	_, _ = unhealthy.PutObject(context.TODO(), "key", strings.NewReader("x"), 1, "", nil)

	core := &backendCore{
		backends: map[string]backend.ObjectBackend{
			"healthy":   healthy,
			"unhealthy": unhealthy,
		},
	}

	eligible := core.excludeUnhealthy([]string{"healthy", "unhealthy"})
	if len(eligible) != 1 {
		t.Fatalf("expected 1 eligible backend, got %d", len(eligible))
	}
	if eligible[0] != "healthy" {
		t.Errorf("expected 'healthy', got %q", eligible[0])
	}
}

// TestExcludeUnhealthy_AllHealthy verifies the exclude unhealthy all healthy contract.
// Asserts that expected 2 eligible backends, got.
func TestExcludeUnhealthy_AllHealthy(t *testing.T) {
	t.Parallel()
	b1 := backend.NewCircuitBreakerBackend(newMockBackend(), "b1", 3, 15*time.Second)
	b2 := backend.NewCircuitBreakerBackend(newMockBackend(), "b2", 3, 15*time.Second)

	core := &backendCore{
		backends: map[string]backend.ObjectBackend{
			"b1": b1,
			"b2": b2,
		},
	}

	eligible := core.excludeUnhealthy([]string{"b1", "b2"})
	if len(eligible) != 2 {
		t.Fatalf("expected 2 eligible backends, got %d", len(eligible))
	}
}

// TestExcludeUnhealthy_AllUnhealthy_TimeoutNotElapsed verifies the exclude unhealthy all unhealthy timeout not elapsed contract.
// Asserts that expected 0 eligible backends before timeout, got.
func TestExcludeUnhealthy_AllUnhealthy_TimeoutNotElapsed(t *testing.T) {
	t.Parallel()
	failingMock1 := newMockBackend()
	failingMock1.putErr = errors.New("backend down")
	b1 := backend.NewCircuitBreakerBackend(failingMock1, "b1", 1, 15*time.Second)

	failingMock2 := newMockBackend()
	failingMock2.putErr = errors.New("backend down")
	b2 := backend.NewCircuitBreakerBackend(failingMock2, "b2", 1, 15*time.Second)

	// Trip both
	_, _ = b1.PutObject(context.TODO(), "key", strings.NewReader("x"), 1, "", nil)
	_, _ = b2.PutObject(context.TODO(), "key", strings.NewReader("x"), 1, "", nil)

	core := &backendCore{
		backends: map[string]backend.ObjectBackend{
			"b1": b1,
			"b2": b2,
		},
	}

	// Before timeout elapses, all should be filtered out
	eligible := core.excludeUnhealthy([]string{"b1", "b2"})
	if len(eligible) != 0 {
		t.Fatalf("expected 0 eligible backends before timeout, got %d", len(eligible))
	}
}

// TestExcludeUnhealthy_AllUnhealthy_ProbeEligible verifies the exclude unhealthy all unhealthy probe eligible contract.
// Asserts that expected 2 probe-eligible backends after timeout, got.
func TestExcludeUnhealthy_AllUnhealthy_ProbeEligible(t *testing.T) {
	t.Parallel()
	failingMock1 := newMockBackend()
	failingMock1.putErr = errors.New("backend down")
	b1 := backend.NewCircuitBreakerBackend(failingMock1, "b1", 1, 1*time.Millisecond)

	failingMock2 := newMockBackend()
	failingMock2.putErr = errors.New("backend down")
	b2 := backend.NewCircuitBreakerBackend(failingMock2, "b2", 1, 1*time.Millisecond)

	// Trip both
	_, _ = b1.PutObject(context.TODO(), "key", strings.NewReader("x"), 1, "", nil)
	_, _ = b2.PutObject(context.TODO(), "key", strings.NewReader("x"), 1, "", nil)

	core := &backendCore{
		backends: map[string]backend.ObjectBackend{
			"b1": b1,
			"b2": b2,
		},
	}

	// Poll until the open timeout elapses and backends become probe-eligible.
	deadline := time.Now().Add(time.Second)
	for {
		eligible := core.excludeUnhealthy([]string{"b1", "b2"})
		if len(eligible) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected 2 probe-eligible backends after timeout, got %d", len(eligible))
		}
		time.Sleep(time.Millisecond)
	}
}

// TestExcludeUnhealthy_HalfOpenAllowedForProbe verifies the exclude unhealthy half open allowed for probe contract.
// Asserts that expected open state, got.
func TestExcludeUnhealthy_HalfOpenAllowedForProbe(t *testing.T) {
	t.Parallel()
	failingMock := newMockBackend()
	failingMock.putErr = errors.New("backend down")
	// Use a tiny open timeout so we can transition to half-open immediately
	b := backend.NewCircuitBreakerBackend(failingMock, "probe", 1, 1*time.Millisecond)

	// Trip the circuit breaker
	_, _ = b.PutObject(context.TODO(), "key", strings.NewReader("x"), 1, "", nil)
	if b.State() != breaker.StateOpen {
		t.Fatalf("expected open state, got %s", b.State())
	}

	// Poll until the open timeout elapses and PreCheck transitions to half-open.
	deadline := time.Now().Add(time.Second)
	for {
		_ = b.PreCheck()
		if b.State() == breaker.StateHalfOpen {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected half-open state, got %s", b.State())
		}
		time.Sleep(time.Millisecond)
	}

	core := &backendCore{
		backends: map[string]backend.ObjectBackend{
			"probe": b,
		},
	}

	eligible := core.excludeUnhealthy([]string{"probe"})
	if len(eligible) != 1 {
		t.Fatalf("expected half-open backend to be eligible for probe, got %d", len(eligible))
	}
}

// TestExcludeUnhealthy_NonCBBackendsAlwaysEligible verifies the exclude unhealthy non cbbackends always eligible contract.
// Asserts that expected 1 eligible backend, got.
func TestExcludeUnhealthy_NonCBBackendsAlwaysEligible(t *testing.T) {
	t.Parallel()
	core := &backendCore{
		backends: map[string]backend.ObjectBackend{
			"plain": newMockBackend(),
		},
	}

	eligible := core.excludeUnhealthy([]string{"plain"})
	if len(eligible) != 1 {
		t.Fatalf("expected 1 eligible backend, got %d", len(eligible))
	}
}

// TestExcludeUnhealthy_UnknownBackendSkipped verifies the exclude unhealthy unknown backend skipped contract.
// Asserts that expected 0 eligible backends, got.
func TestExcludeUnhealthy_UnknownBackendSkipped(t *testing.T) {
	t.Parallel()
	core := &backendCore{
		backends: map[string]backend.ObjectBackend{},
	}

	eligible := core.excludeUnhealthy([]string{"missing"})
	if len(eligible) != 0 {
		t.Fatalf("expected 0 eligible backends, got %d", len(eligible))
	}
}

// -------------------------------------------------------------------------
// withTimeout  -  deadline cascading
// -------------------------------------------------------------------------

// TestWithTimeout_NoParentDeadline verifies the with timeout no parent deadline contract.
// Asserts that expected ~5s deadline, got.
func TestWithTimeout_NoParentDeadline(t *testing.T) {
	t.Parallel()
	core := &backendCore{backendTimeout: 5 * time.Second}
	ctx, cancel := core.withTimeout(context.Background())
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected deadline to be set")
	}
	remaining := time.Until(deadline)
	if remaining < 4*time.Second || remaining > 6*time.Second {
		t.Errorf("expected ~5s deadline, got %v", remaining)
	}
}

// TestWithTimeout_ParentTighter verifies the with timeout parent tighter contract.
// Asserts that expected parent's ~1s deadline to be preserved, got.
func TestWithTimeout_ParentTighter(t *testing.T) {
	t.Parallel()
	// Parent has a 1s deadline; backend timeout is 30s.
	// The tighter parent deadline should be preserved.
	parent, parentCancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer parentCancel()

	core := &backendCore{backendTimeout: 30 * time.Second}
	ctx, cancel := core.withTimeout(parent)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected deadline to be set")
	}
	remaining := time.Until(deadline)
	if remaining > 2*time.Second {
		t.Errorf("expected parent's ~1s deadline to be preserved, got %v", remaining)
	}
}

// TestWithTimeout_BackendTighter verifies the with timeout backend tighter contract.
// Asserts that expected backend's ~1s timeout to be applied, got.
func TestWithTimeout_BackendTighter(t *testing.T) {
	t.Parallel()
	// Parent has a 30s deadline; backend timeout is 1s.
	// The tighter backend timeout should be applied.
	parent, parentCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer parentCancel()

	core := &backendCore{backendTimeout: 1 * time.Second}
	ctx, cancel := core.withTimeout(parent)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected deadline to be set")
	}
	remaining := time.Until(deadline)
	if remaining > 2*time.Second {
		t.Errorf("expected backend's ~1s timeout to be applied, got %v", remaining)
	}
}

// TestWithTimeout_ZeroTimeout verifies the with timeout zero timeout path by exercising context.Background, ctx.Deadline.
func TestWithTimeout_ZeroTimeout(t *testing.T) {
	t.Parallel()
	core := &backendCore{backendTimeout: 0}
	ctx, cancel := core.withTimeout(context.Background())
	defer cancel()

	if _, ok := ctx.Deadline(); ok {
		t.Error("expected no deadline when backendTimeout is 0")
	}
}

// -------------------------------------------------------------------------
// acquireAdmission / releaseAdmission
// -------------------------------------------------------------------------

// TestAcquireAdmission_NilSem verifies the acquire admission nil sem path by exercising context.Background.
func TestAcquireAdmission_NilSem(t *testing.T) {
	t.Parallel()
	core := &backendCore{}
	if !core.acquireAdmission(context.Background()) {
		t.Error("nil semaphore should always succeed")
	}
	core.releaseAdmission() // should not panic
}

// TestAcquireAdmission_Bounded verifies the acquire admission bounded path by exercising context.Background, context.WithCancel.
func TestAcquireAdmission_Bounded(t *testing.T) {
	t.Parallel()
	sem := make(chan struct{}, 1)
	core := &backendCore{admissionSem: sem}

	// First acquire succeeds
	if !core.acquireAdmission(context.Background()) {
		t.Fatal("first acquire should succeed")
	}

	// Second acquire should block; use a cancelled context to prove it
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if core.acquireAdmission(ctx) {
		t.Error("acquire on full semaphore with cancelled context should return false")
	}

	// Release and re-acquire
	core.releaseAdmission()
	if !core.acquireAdmission(context.Background()) {
		t.Error("acquire after release should succeed")
	}
	core.releaseAdmission()
}

// TestEligibleForWrite_CombinesAllFilters verifies the eligible for write combines all filters contract.
// Asserts that eligibleForWrite = , want [healthy].
func TestEligibleForWrite_CombinesAllFilters(t *testing.T) {
	t.Parallel()
	// healthy: passes all checks
	healthy := newMockBackend()

	// draining: excluded by drain check
	draining := newMockBackend()

	// unhealthy: circuit breaker is open
	failingMock := newMockBackend()
	failingMock.putErr = errors.New("down")
	unhealthy := backend.NewCircuitBreakerBackend(failingMock, "unhealthy", 1, 30*time.Second)
	_, _ = unhealthy.PutObject(context.TODO(), "k", strings.NewReader("x"), 1, "", nil) // trip breaker

	// over-limit: within limits check fails
	overLimit := newMockBackend()

	limits := map[string]core.UsageLimits{
		"over-limit": {APIRequestLimit: 1},
	}
	usage := counter.NewUsageTracker(
		counter.NewLocalCounterBackend([]string{"healthy", "draining", "unhealthy", "over-limit"}),
		limits,
	)
	usage.SetBaseline("over-limit", core.UsageStat{APIRequests: 1})

	dm := drain.New(nil, nil, nil, nil, nil, nil)
	dm.SeedActiveForTest("draining")
	core := &backendCore{
		backends: map[string]backend.ObjectBackend{
			"healthy":    healthy,
			"draining":   draining,
			"unhealthy":  unhealthy,
			"over-limit": overLimit,
		},
		order:    []string{"healthy", "draining", "unhealthy", "over-limit"},
		usage:    usage,
		drainMgr: dm,
	}

	eligible := core.eligibleForWrite(1, 0, 0)
	if len(eligible) != 1 || eligible[0] != "healthy" {
		t.Errorf("eligibleForWrite = %v, want [healthy]", eligible)
	}
}

// TestEligibleForWrite_MaxObjectSize verifies the eligible for write max object size contract.
// Asserts that 10MB object: eligible = , want all 3 backends.
func TestEligibleForWrite_MaxObjectSize(t *testing.T) {
	t.Parallel()

	usage := counter.NewUsageTracker(
		counter.NewLocalCounterBackend([]string{"small", "large", "unlimited"}),
		nil,
	)

	core := &backendCore{
		backends: map[string]backend.ObjectBackend{
			"small":     newMockBackend(),
			"large":     newMockBackend(),
			"unlimited": newMockBackend(),
		},
		order:          []string{"small", "large", "unlimited"},
		usage:          usage,
		maxObjectSizes: map[string]int64{"small": 50 * 1024 * 1024}, // 50 MB
	}

	// Object under the limit  -  all backends eligible
	eligible := core.eligibleForWrite(1, 0, 10*1024*1024)
	if len(eligible) != 3 {
		t.Errorf("10MB object: eligible = %v, want all 3 backends", eligible)
	}

	// Object over small's limit  -  small excluded
	eligible = core.eligibleForWrite(1, 0, 100*1024*1024)
	if len(eligible) != 2 {
		t.Errorf("100MB object: eligible = %v, want [large, unlimited]", eligible)
	}
	for _, name := range eligible {
		if name == "small" {
			t.Error("100MB object: 'small' should be excluded (max_object_size=50MB)")
		}
	}
}
