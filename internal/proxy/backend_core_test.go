// -------------------------------------------------------------------------------
// Backend Core Tests - Circuit Breaker Filtering
//
// Author: Alex Freidah
//
// Unit tests for ExcludeUnhealthy which filters circuit-broken backends from
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
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	storecore "github.com/afreidah/s3-orchestrator/internal/store/core"
)

// TestBackendCore_LogFallback covers the nil-safe behaviour of Log():
// when *infra.Core was constructed without a log field, the helper returns
// slog.Default() so callers never dereference a nil logger.
func TestBackendCore_LogFallback(t *testing.T) {
	t.Parallel()
	c := infra.New(&infra.Config{})
	if c.Log() == nil {
		t.Fatal("Log() returned nil for zero-config Core")
	}
}

// TestExcludeUnhealthy_FiltersOpenCircuitBreaker verifies the exclude unhealthy filters open circuit breaker contract.
// Asserts that expected 1 eligible backend, got.
func TestExcludeUnhealthy_FiltersOpenCircuitBreaker(t *testing.T) {
	t.Parallel()
	healthy := backend.NewCircuitBreakerBackend(newMockBackend(), "healthy", 3, 15*time.Second)

	failingMock := newMockBackend()
	failingMock.putErr = errors.New("backend down")
	unhealthy := backend.NewCircuitBreakerBackend(failingMock, "unhealthy", 1, 15*time.Second)

	_, _ = unhealthy.PutObject(context.TODO(), "key", strings.NewReader("x"), 1, "", nil)

	c := infra.New(&infra.Config{
		Backends: map[string]backend.ObjectBackend{
			"healthy":   healthy,
			"unhealthy": unhealthy,
		},
	})

	eligible := c.ExcludeUnhealthy([]string{"healthy", "unhealthy"})
	if len(eligible) != 1 {
		t.Fatalf("expected 1 eligible backend, got %d", len(eligible))
	}
	if eligible[0] != "healthy" {
		t.Errorf("expected 'healthy', got %q", eligible[0])
	}
}

// TestExcludeUnhealthy_AllHealthy verifies the exclude unhealthy all healthy contract.
func TestExcludeUnhealthy_AllHealthy(t *testing.T) {
	t.Parallel()
	b1 := backend.NewCircuitBreakerBackend(newMockBackend(), "b1", 3, 15*time.Second)
	b2 := backend.NewCircuitBreakerBackend(newMockBackend(), "b2", 3, 15*time.Second)

	c := infra.New(&infra.Config{
		Backends: map[string]backend.ObjectBackend{
			"b1": b1,
			"b2": b2,
		},
	})

	eligible := c.ExcludeUnhealthy([]string{"b1", "b2"})
	if len(eligible) != 2 {
		t.Fatalf("expected 2 eligible backends, got %d", len(eligible))
	}
}

// TestExcludeUnhealthy_AllUnhealthy_TimeoutNotElapsed verifies the timeout-not-elapsed contract.
func TestExcludeUnhealthy_AllUnhealthy_TimeoutNotElapsed(t *testing.T) {
	t.Parallel()
	failingMock1 := newMockBackend()
	failingMock1.putErr = errors.New("backend down")
	b1 := backend.NewCircuitBreakerBackend(failingMock1, "b1", 1, 15*time.Second)

	failingMock2 := newMockBackend()
	failingMock2.putErr = errors.New("backend down")
	b2 := backend.NewCircuitBreakerBackend(failingMock2, "b2", 1, 15*time.Second)

	_, _ = b1.PutObject(context.TODO(), "key", strings.NewReader("x"), 1, "", nil)
	_, _ = b2.PutObject(context.TODO(), "key", strings.NewReader("x"), 1, "", nil)

	c := infra.New(&infra.Config{
		Backends: map[string]backend.ObjectBackend{
			"b1": b1,
			"b2": b2,
		},
	})

	eligible := c.ExcludeUnhealthy([]string{"b1", "b2"})
	if len(eligible) != 0 {
		t.Fatalf("expected 0 eligible backends before timeout, got %d", len(eligible))
	}
}

// TestExcludeUnhealthy_AllUnhealthy_ProbeEligible verifies the probe-eligible contract.
func TestExcludeUnhealthy_AllUnhealthy_ProbeEligible(t *testing.T) {
	t.Parallel()
	failingMock1 := newMockBackend()
	failingMock1.putErr = errors.New("backend down")
	b1 := backend.NewCircuitBreakerBackend(failingMock1, "b1", 1, 1*time.Millisecond)

	failingMock2 := newMockBackend()
	failingMock2.putErr = errors.New("backend down")
	b2 := backend.NewCircuitBreakerBackend(failingMock2, "b2", 1, 1*time.Millisecond)

	_, _ = b1.PutObject(context.TODO(), "key", strings.NewReader("x"), 1, "", nil)
	_, _ = b2.PutObject(context.TODO(), "key", strings.NewReader("x"), 1, "", nil)

	c := infra.New(&infra.Config{
		Backends: map[string]backend.ObjectBackend{
			"b1": b1,
			"b2": b2,
		},
	})

	deadline := time.Now().Add(time.Second)
	for {
		eligible := c.ExcludeUnhealthy([]string{"b1", "b2"})
		if len(eligible) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected 2 probe-eligible backends after timeout, got %d", len(eligible))
		}
		time.Sleep(time.Millisecond)
	}
}

// TestExcludeUnhealthy_HalfOpenAllowedForProbe verifies the half-open probe contract.
func TestExcludeUnhealthy_HalfOpenAllowedForProbe(t *testing.T) {
	t.Parallel()
	failingMock := newMockBackend()
	failingMock.putErr = errors.New("backend down")
	b := backend.NewCircuitBreakerBackend(failingMock, "probe", 1, 1*time.Millisecond)

	_, _ = b.PutObject(context.TODO(), "key", strings.NewReader("x"), 1, "", nil)
	if b.State() != breaker.StateOpen {
		t.Fatalf("expected open state, got %s", b.State())
	}

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

	c := infra.New(&infra.Config{
		Backends: map[string]backend.ObjectBackend{
			"probe": b,
		},
	})

	eligible := c.ExcludeUnhealthy([]string{"probe"})
	if len(eligible) != 1 {
		t.Fatalf("expected half-open backend to be eligible for probe, got %d", len(eligible))
	}
}

// TestExcludeUnhealthy_NonCBBackendsAlwaysEligible verifies non-CB backends are always eligible.
func TestExcludeUnhealthy_NonCBBackendsAlwaysEligible(t *testing.T) {
	t.Parallel()
	c := infra.New(&infra.Config{
		Backends: map[string]backend.ObjectBackend{
			"plain": newMockBackend(),
		},
	})

	eligible := c.ExcludeUnhealthy([]string{"plain"})
	if len(eligible) != 1 {
		t.Fatalf("expected 1 eligible backend, got %d", len(eligible))
	}
}

// TestExcludeUnhealthy_UnknownBackendSkipped verifies unknown backends are skipped.
func TestExcludeUnhealthy_UnknownBackendSkipped(t *testing.T) {
	t.Parallel()
	c := infra.New(&infra.Config{
		Backends: map[string]backend.ObjectBackend{},
	})

	eligible := c.ExcludeUnhealthy([]string{"missing"})
	if len(eligible) != 0 {
		t.Fatalf("expected 0 eligible backends, got %d", len(eligible))
	}
}

// -------------------------------------------------------------------------
// WithTimeout  -  deadline cascading
// -------------------------------------------------------------------------

func TestWithTimeout_NoParentDeadline(t *testing.T) {
	t.Parallel()
	c := infra.New(&infra.Config{BackendTimeout: 5 * time.Second})
	ctx, cancel := c.WithTimeout(context.Background())
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

func TestWithTimeout_ParentTighter(t *testing.T) {
	t.Parallel()
	parent, parentCancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer parentCancel()

	c := infra.New(&infra.Config{BackendTimeout: 30 * time.Second})
	ctx, cancel := c.WithTimeout(parent)
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

func TestWithTimeout_BackendTighter(t *testing.T) {
	t.Parallel()
	parent, parentCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer parentCancel()

	c := infra.New(&infra.Config{BackendTimeout: 1 * time.Second})
	ctx, cancel := c.WithTimeout(parent)
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

func TestWithTimeout_ZeroTimeout(t *testing.T) {
	t.Parallel()
	c := infra.New(&infra.Config{BackendTimeout: 0})
	ctx, cancel := c.WithTimeout(context.Background())
	defer cancel()

	if _, ok := ctx.Deadline(); ok {
		t.Error("expected no deadline when backendTimeout is 0")
	}
}

// -------------------------------------------------------------------------
// AcquireAdmission / ReleaseAdmission
// -------------------------------------------------------------------------

func TestAcquireAdmission_NilSem(t *testing.T) {
	t.Parallel()
	c := infra.New(&infra.Config{})
	if !c.AcquireAdmission(context.Background()) {
		t.Error("nil semaphore should always succeed")
	}
	c.ReleaseAdmission() // should not panic
}

func TestAcquireAdmission_Bounded(t *testing.T) {
	t.Parallel()
	sem := make(chan struct{}, 1)
	c := infra.New(&infra.Config{AdmissionSem: sem})

	if !c.AcquireAdmission(context.Background()) {
		t.Fatal("first acquire should succeed")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if c.AcquireAdmission(ctx) {
		t.Error("acquire on full semaphore with cancelled context should return false")
	}

	c.ReleaseAdmission()
	if !c.AcquireAdmission(context.Background()) {
		t.Error("acquire after release should succeed")
	}
	c.ReleaseAdmission()
}

func TestEligibleForWrite_CombinesAllFilters(t *testing.T) {
	t.Parallel()
	healthy := newMockBackend()
	draining := newMockBackend()

	failingMock := newMockBackend()
	failingMock.putErr = errors.New("down")
	unhealthy := backend.NewCircuitBreakerBackend(failingMock, "unhealthy", 1, 30*time.Second)
	_, _ = unhealthy.PutObject(context.TODO(), "k", strings.NewReader("x"), 1, "", nil)

	overLimit := newMockBackend()

	limits := map[string]storecore.UsageLimits{
		"over-limit": {APIRequestLimit: 1},
	}
	usage := counter.NewUsageTracker(
		counter.NewLocalCounterBackend([]string{"healthy", "draining", "unhealthy", "over-limit"}),
		limits,
	)
	usage.SetBaseline("over-limit", storecore.UsageStat{APIRequests: 1})

	dm := drain.New(nil, nil, nil, nil, nil, nil)
	dm.SeedActiveForTest("draining")
	c := infra.New(&infra.Config{
		Backends: map[string]backend.ObjectBackend{
			"healthy":    healthy,
			"draining":   draining,
			"unhealthy":  unhealthy,
			"over-limit": overLimit,
		},
		Order: []string{"healthy", "draining", "unhealthy", "over-limit"},
		Usage: usage,
	})
	c.SetDrainChecker(dm)

	eligible := c.EligibleForWrite(1, 0, 0)
	if len(eligible) != 1 || eligible[0] != "healthy" {
		t.Errorf("EligibleForWrite = %v, want [healthy]", eligible)
	}
}

func TestEligibleForWrite_MaxObjectSize(t *testing.T) {
	t.Parallel()

	usage := counter.NewUsageTracker(
		counter.NewLocalCounterBackend([]string{"small", "large", "unlimited"}),
		nil,
	)

	c := infra.New(&infra.Config{
		Backends: map[string]backend.ObjectBackend{
			"small":     newMockBackend(),
			"large":     newMockBackend(),
			"unlimited": newMockBackend(),
		},
		Order:          []string{"small", "large", "unlimited"},
		Usage:          usage,
		MaxObjectSizes: map[string]int64{"small": 50 * 1024 * 1024},
	})

	eligible := c.EligibleForWrite(1, 0, 10*1024*1024)
	if len(eligible) != 3 {
		t.Errorf("10MB object: eligible = %v, want all 3 backends", eligible)
	}

	eligible = c.EligibleForWrite(1, 0, 100*1024*1024)
	if len(eligible) != 2 {
		t.Errorf("100MB object: eligible = %v, want [large, unlimited]", eligible)
	}
	for _, name := range eligible {
		if name == "small" {
			t.Error("100MB object: 'small' should be excluded (max_object_size=50MB)")
		}
	}
}
