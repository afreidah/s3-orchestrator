// -------------------------------------------------------------------------------
// Infra Capability Tests
//
// Author: Alex Freidah
//
// One focused test per capability the *BackendRuntime composes. Each test
// exercises the capability in isolation (no BackendRuntime construction, no
// metrics collector, no DB) so a regression in one capability's
// behavior is localised at the failure site instead of surfacing
// through an integration test in a sibling package.
// -------------------------------------------------------------------------------

package infra

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace/noop"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// backendRegistry
// -------------------------------------------------------------------------

// staticDrainChecker is a fixed-set drain checker the registry test
// uses to drive ExcludeDraining without standing up drain.Manager.
type staticDrainChecker map[string]bool

func (s staticDrainChecker) IsDraining(name string) bool { return s[name] }

// stubBackend satisfies backend.ObjectBackend at the interface level
// so registry.All() / Get() have something to return. None of its
// methods are exercised in this file.
type stubBackend struct{ backend.ObjectBackend }

func TestBackendRegistry_GetAndAllAndOrder(t *testing.T) {
	t.Parallel()
	b1, b2 := &stubBackend{}, &stubBackend{}
	r := newBackendRegistry(map[string]backend.ObjectBackend{"b1": b1, "b2": b2}, []string{"b1", "b2"})

	got, err := r.Get("b1")
	if err != nil || got != b1 {
		t.Errorf("Get(b1) = %v, %v; want %p, nil", got, err, b1)
	}
	if _, err := r.Get("nope"); err == nil {
		t.Error("Get(nope) returned no error")
	}
	if len(r.All()) != 2 {
		t.Errorf("All len = %d, want 2", len(r.All()))
	}
	if len(r.Order()) != 2 || r.Order()[0] != "b1" {
		t.Errorf("Order = %v, want [b1 b2]", r.Order())
	}
}

func TestBackendRegistry_ExcludeDraining_NoCheckerWiredKeepsAll(t *testing.T) {
	t.Parallel()
	r := newBackendRegistry(nil, []string{"a", "b"})
	got := r.ExcludeDraining([]string{"a", "b"})
	if len(got) != 2 {
		t.Errorf("ExcludeDraining = %v, want both (no checker wired)", got)
	}
}

func TestBackendRegistry_ExcludeDraining_DropsActiveDrains(t *testing.T) {
	t.Parallel()
	r := newBackendRegistry(nil, []string{"a", "b", "c"})
	r.SetDrainChecker(staticDrainChecker{"b": true})
	got := r.ExcludeDraining([]string{"a", "b", "c"})
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Errorf("ExcludeDraining = %v, want [a c]", got)
	}
}

// -------------------------------------------------------------------------
// usagePolicy
// -------------------------------------------------------------------------

func TestUsagePolicy_WithinLimits_MaxObjectSizeEnforced(t *testing.T) {
	t.Parallel()
	tracker := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1", "b2", "b3"}), nil)
	// No limits => baseline check is always within; max-object-size is
	// the only thing that can reject ingress here.
	p := newUsagePolicy(tracker, map[string]int64{"b1": 1024})

	if !p.WithinLimits("b1", 1, 0, 512) {
		t.Error("WithinLimits = false for 512 < 1024 max; want true")
	}
	if p.WithinLimits("b1", 1, 0, 2048) {
		t.Error("WithinLimits = true for 2048 > 1024 max; want false")
	}
}

func TestUsagePolicy_FilterEligible_PreservesInputOrder(t *testing.T) {
	t.Parallel()
	tracker := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1", "b2", "b3"}), nil)
	p := newUsagePolicy(tracker, map[string]int64{"b2": 100})

	got := p.FilterEligible([]string{"b1", "b2", "b3"}, 1, 0, 500)
	// b2 is dropped (500 > 100 max); b1, b3 stay in original order.
	if len(got) != 2 || got[0] != "b1" || got[1] != "b3" {
		t.Errorf("FilterEligible = %v, want [b1 b3]", got)
	}
}

// -------------------------------------------------------------------------
// timeoutPolicy
// -------------------------------------------------------------------------

func TestTimeoutPolicy_WithTimeout_RespectsTighterParentDeadline(t *testing.T) {
	t.Parallel()
	p := newTimeoutPolicy(1 * time.Hour)
	parent, parentCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer parentCancel()

	ctx, cancel := p.WithTimeout(parent)
	defer cancel()

	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("no deadline on returned ctx")
	}
	if time.Until(dl) > 100*time.Millisecond {
		t.Errorf("returned deadline %v not tighter than parent's 50ms", time.Until(dl))
	}
}

func TestTimeoutPolicy_WithTimeout_NoTimeoutReturnsRealCancel(t *testing.T) {
	t.Parallel()
	p := newTimeoutPolicy(0)
	ctx, cancel := p.WithTimeout(context.Background())
	if ctx == nil || cancel == nil {
		t.Fatal("expected non-nil ctx and cancel even when timeout is 0")
	}
	// Verify cancel actually cancels (so callers can defer it without
	// the empty-func style we banned in the style guide).
	cancel()
	if ctx.Err() == nil {
		t.Error("cancel() did not cancel the context")
	}
}

// -------------------------------------------------------------------------
// errorClassifier
// -------------------------------------------------------------------------

func TestErrorClassifier_MapsDBUnavailableToServiceUnavailable(t *testing.T) {
	t.Parallel()
	c := newErrorClassifier()
	_, span := noop.NewTracerProvider().Tracer("t").Start(context.Background(), "s")
	got := c.ClassifyWriteError(span, "PutObject", core.ErrDBUnavailable)
	if !errors.Is(got, core.ErrServiceUnavailable) {
		t.Errorf("got %v, want ErrServiceUnavailable", got)
	}
}

func TestErrorClassifier_MapsNoSpaceToInsufficientStorage(t *testing.T) {
	t.Parallel()
	c := newErrorClassifier()
	_, span := noop.NewTracerProvider().Tracer("t").Start(context.Background(), "s")
	got := c.ClassifyWriteError(span, "PutObject", core.ErrNoSpaceAvailable)
	if !errors.Is(got, core.ErrInsufficientStorage) {
		t.Errorf("got %v, want ErrInsufficientStorage", got)
	}
}

func TestErrorClassifier_PassesThroughUnknownErrors(t *testing.T) {
	t.Parallel()
	c := newErrorClassifier()
	_, span := noop.NewTracerProvider().Tracer("t").Start(context.Background(), "s")
	in := errors.New("kaboom")
	got := c.ClassifyWriteError(span, "PutObject", in)
	if !errors.Is(got, in) {
		t.Errorf("got %v, want chain containing %v", got, in)
	}
}

// -------------------------------------------------------------------------
// admissionGate
// -------------------------------------------------------------------------

func TestAdmissionGate_NoSemAcquireAlwaysSucceeds(t *testing.T) {
	t.Parallel()
	g := newAdmissionGate(nil)
	if !g.Acquire(context.Background()) {
		t.Error("Acquire returned false with no sem wired")
	}
	g.Release() // must not panic
}

func TestAdmissionGate_BoundedSemBlocksWhenFull(t *testing.T) {
	t.Parallel()
	g := newAdmissionGate(make(chan struct{}, 1))
	if !g.Acquire(context.Background()) {
		t.Fatal("first Acquire failed")
	}
	// Second Acquire should fail when ctx is already cancelled.
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if g.Acquire(cctx) {
		t.Error("Acquire succeeded against a full sem with cancelled ctx")
	}
	g.Release()
	// After Release the slot should be reusable.
	if !g.Acquire(context.Background()) {
		t.Error("Acquire after Release failed")
	}
}
