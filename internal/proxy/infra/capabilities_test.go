// -------------------------------------------------------------------------------
// Infra Capability Tests
//
// Author: Alex Freidah
//
// Focused tests for each concern *BackendRuntime covers - registry, usage
// limits, timeouts, error classification, admission - each on a runtime
// holding only the fields that concern reads, with no metrics collector or DB.
// -------------------------------------------------------------------------------

package infra

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace/noop"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// backendRegistry
// -------------------------------------------------------------------------

// staticFlags is a fixed set of backend flags the registry tests use to drive
// the state filters without standing up backendstate.Manager.
type staticFlags map[string]core.BackendFlags

func (s staticFlags) Flags(name string) core.BackendFlags { return s[name] }

// stubBackend satisfies backend.ObjectBackend at the interface level
// so registry.All() / Get() have something to return. None of its
// methods are exercised in this file.
type stubBackend struct{ backend.ObjectBackend }

func TestBackendRegistry_GetAndAllAndOrder(t *testing.T) {
	t.Parallel()
	b1, b2 := &stubBackend{}, &stubBackend{}
	r := &BackendRuntime{backends: map[string]backend.ObjectBackend{"b1": b1, "b2": b2}, order: []string{"b1", "b2"}}

	got, err := r.GetBackend("b1")
	if err != nil || got != b1 {
		t.Errorf("GetBackend(b1) = %v, %v; want %p, nil", got, err, b1)
	}
	if _, err := r.GetBackend("nope"); err == nil {
		t.Error("GetBackend(nope) returned no error")
	}
	if len(r.Backends()) != 2 {
		t.Errorf("Backends len = %d, want 2", len(r.Backends()))
	}
	if len(r.BackendOrder()) != 2 || r.BackendOrder()[0] != "b1" {
		t.Errorf("BackendOrder = %v, want [b1 b2]", r.BackendOrder())
	}
}

func TestBackendRegistry_Accepted_NoCheckerWiredKeepsAll(t *testing.T) {
	t.Parallel()
	r := &BackendRuntime{order: []string{"a", "b"}}
	got := r.Accepted([]string{"a", "b"}, core.AcceptForWrite)
	if len(got) != 2 {
		t.Errorf("Accepted = %v, want both (no checker wired)", got)
	}
}

// TestBackendRegistry_Accepted_AppliesTheMask verifies a cordoned backend is
// dropped for writes but kept for reconcile, and a draining one is dropped for
// both.
func TestBackendRegistry_Accepted_AppliesTheMask(t *testing.T) {
	t.Parallel()
	r := &BackendRuntime{order: []string{"a", "b", "c"}}
	r.SetStateChecker(staticFlags{"b": core.FlagCordoned, "c": core.FlagDraining})
	if got := r.Accepted([]string{"a", "b", "c"}, core.AcceptForWrite); len(got) != 1 || got[0] != "a" {
		t.Errorf("Accepted for write = %v, want [a]", got)
	}
	if got := r.Accepted([]string{"a", "b", "c"}, core.AcceptForReconcile); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("Accepted for reconcile = %v, want [a b]", got)
	}
}

// -------------------------------------------------------------------------
// usagePolicy
// -------------------------------------------------------------------------

func TestUsagePolicy_EligibleForWrite_PreservesOrder(t *testing.T) {
	t.Parallel()
	tracker := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1", "b2", "b3"}), nil)
	p := &BackendRuntime{
		backends: map[string]backend.ObjectBackend{"b1": &stubBackend{}, "b2": &stubBackend{}, "b3": &stubBackend{}},
		order:    []string{"b1", "b2", "b3"},
		usage:    tracker,
		profiles: map[string]core.BackendProfile{"b2": {MaxObjectSize: 100}},
	}

	got := p.EligibleForWrite(s3op.PutObject, 0, 500)
	// b2 is dropped (500 > 100 max); b1, b3 stay in configured order.
	if len(got) != 2 || got[0] != "b1" || got[1] != "b3" {
		t.Errorf("EligibleForWrite = %v, want [b1 b3]", got)
	}
}

// -------------------------------------------------------------------------
// timeoutPolicy
// -------------------------------------------------------------------------

func TestTimeoutPolicy_WithTimeout_RespectsTighterParentDeadline(t *testing.T) {
	t.Parallel()
	p := &BackendRuntime{backendTimeout: time.Hour}
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
	p := &BackendRuntime{}
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

// fakeTimeoutBackend captures the context of the last GET/HEAD call so the
// lifecycle tests can assert the per-call timeout context is released (or still
// live) at the right moment. A nil getErr/headErr yields a minimal success.
type fakeTimeoutBackend struct {
	backend.ObjectBackend
	gotCtx  context.Context
	getErr  error
	headErr error
}

func (f *fakeTimeoutBackend) GetObject(ctx context.Context, _, _ string) (*backend.GetObjectResult, error) {
	f.gotCtx = ctx
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &backend.GetObjectResult{Body: io.NopCloser(strings.NewReader("ok")), Size: 2}, nil
}

func (f *fakeTimeoutBackend) HeadObject(ctx context.Context, _ string) (*backend.HeadObjectResult, error) {
	f.gotCtx = ctx
	if f.headErr != nil {
		return nil, f.headErr
	}
	return &backend.HeadObjectResult{Size: 2}, nil
}

func TestTimeoutPolicy_Get_SuccessHandsBackLiveCancel(t *testing.T) {
	t.Parallel()
	p, counters := newCopyRuntime(t)
	be := &fakeTimeoutBackend{}

	result, cancel, err := p.Get(context.Background(), "b1", be, "k", "")
	if err != nil {
		t.Fatalf("Get err = %v, want nil", err)
	}
	// A successful GET's egress is the reader's to charge.
	if got := loadUsage(counters, "b1"); got != (usageOf{}) {
		t.Errorf("successful GET charged %+v, want nothing until the reader charges egress", got)
	}
	if result == nil || result.Body == nil {
		t.Fatal("expected non-nil result with body")
	}
	defer result.Body.Close()
	// The body's lifetime is the caller's: the context stays live until cancel.
	if be.gotCtx.Err() != nil {
		t.Error("call context cancelled before caller released it")
	}
	cancel()
	if be.gotCtx.Err() == nil {
		t.Error("cancel() did not release the call context")
	}
}

func TestTimeoutPolicy_Get_ErrorReleasesContextAndCharges(t *testing.T) {
	t.Parallel()
	p, counters := newCopyRuntime(t)
	wantErr := errors.New("boom")
	be := &fakeTimeoutBackend{getErr: wantErr}

	result, cancel, err := p.Get(context.Background(), "b1", be, "k", "")
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if result != nil || cancel != nil {
		t.Fatalf("on error want nil result and cancel, got %v, %v", result, cancel)
	}
	// No body to own on error, so the context must already be released.
	if be.gotCtx.Err() == nil {
		t.Error("error path left the call context un-cancelled")
	}
	if got := loadUsage(counters, "b1"); got != (usageOf{calls: 1}) {
		t.Errorf("failed GET charged %+v, want one call", got)
	}
}

func TestTimeoutPolicy_Get_RespectsTighterParentDeadline(t *testing.T) {
	t.Parallel()
	p, _ := newCopyRuntime(t)
	be := &fakeTimeoutBackend{}
	parent, parentCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer parentCancel()

	_, cancel, err := p.Get(parent, "b1", be, "k", "")
	if err != nil {
		t.Fatalf("Get err = %v", err)
	}
	defer cancel()
	dl, ok := be.gotCtx.Deadline()
	if !ok || time.Until(dl) > 100*time.Millisecond {
		t.Errorf("call deadline not tightened to parent's 50ms (ok=%v)", ok)
	}
}

// TestTimeoutPolicy_Head_ChargesEveryCall verifies a HEAD releases its context
// on return and is charged one call whether or not it succeeded.
func TestTimeoutPolicy_Head_ChargesEveryCall(t *testing.T) {
	t.Parallel()
	for _, headErr := range []error{nil, errors.New("nope")} {
		p, counters := newCopyRuntime(t)
		be := &fakeTimeoutBackend{headErr: headErr}

		result, err := p.Head(context.Background(), "b1", be, "k")
		if !errors.Is(err, headErr) || (headErr == nil && result == nil) {
			t.Fatalf("Head = %v, %v; want the backend's answer", result, err)
		}
		if be.gotCtx.Err() == nil {
			t.Error("Head did not release the call context on return")
		}
		if got := loadUsage(counters, "b1"); got != (usageOf{calls: 1}) {
			t.Errorf("HEAD (err %v) charged %+v, want one call", headErr, got)
		}
	}
}

// TestTimeoutPolicy_Put_ChargesWhatLanded verifies a PUT that lands is charged
// its ingress and one that fails a bare call.
func TestTimeoutPolicy_Put_ChargesWhatLanded(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		dst  backend.ObjectBackend
		want usageOf
	}{
		{"landed", &recordingBackend{}, usageOf{calls: 1, ingress: 5}},
		{"refused", writeRejectingBackend{}, usageOf{calls: 1}},
	}
	for _, tc := range cases {
		p, counters := newCopyRuntime(t)
		_, err := p.Put(context.Background(), s3op.PutObject, &backend.PutRequest{
			Dst: ep("b2", tc.dst), Key: "k", Body: strings.NewReader("hello"), Size: 5,
		})
		if (err != nil) != (tc.want.ingress == 0) {
			t.Errorf("%s: err = %v", tc.name, err)
		}
		if got := loadUsage(counters, "b2"); got != tc.want {
			t.Errorf("%s: charged %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// -------------------------------------------------------------------------
// errorClassifier
// -------------------------------------------------------------------------

func TestErrorClassifier_MapsDBUnavailableToServiceUnavailable(t *testing.T) {
	t.Parallel()
	c := &BackendRuntime{}
	_, span := noop.NewTracerProvider().Tracer("t").Start(context.Background(), "s")
	got := c.ClassifyWriteError(span, s3op.PutObject, core.ErrDBUnavailable)
	if !errors.Is(got, core.ErrServiceUnavailable) {
		t.Errorf("got %v, want ErrServiceUnavailable", got)
	}
}

func TestErrorClassifier_KeepsInsufficientStorage(t *testing.T) {
	t.Parallel()
	c := &BackendRuntime{}
	_, span := noop.NewTracerProvider().Tracer("t").Start(context.Background(), "s")
	got := c.ClassifyWriteError(span, s3op.PutObject, core.ErrInsufficientStorage)
	if !errors.Is(got, core.ErrInsufficientStorage) {
		t.Errorf("got %v, want ErrInsufficientStorage", got)
	}
}

func TestErrorClassifier_PassesThroughUnknownErrors(t *testing.T) {
	t.Parallel()
	c := &BackendRuntime{}
	_, span := noop.NewTracerProvider().Tracer("t").Start(context.Background(), "s")
	in := errors.New("kaboom")
	got := c.ClassifyWriteError(span, s3op.PutObject, in)
	if !errors.Is(got, in) {
		t.Errorf("got %v, want chain containing %v", got, in)
	}
}

// -------------------------------------------------------------------------
// admissionGate
// -------------------------------------------------------------------------

func TestAdmissionGate_NoSemAcquireAlwaysSucceeds(t *testing.T) {
	t.Parallel()
	g := &BackendRuntime{}
	if !g.AcquireAdmission(context.Background()) {
		t.Error("AcquireAdmission returned false with no sem wired")
	}
	g.ReleaseAdmission() // must not panic
}

func TestAdmissionGate_BoundedSemBlocksWhenFull(t *testing.T) {
	t.Parallel()
	g := &BackendRuntime{admissionSem: make(chan struct{}, 1)}
	if !g.AcquireAdmission(context.Background()) {
		t.Fatal("first AcquireAdmission failed")
	}
	// A second acquire fails when ctx is already cancelled.
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if g.AcquireAdmission(cctx) {
		t.Error("AcquireAdmission succeeded against a full sem with cancelled ctx")
	}
	g.ReleaseAdmission()
	// After a release the slot is reusable.
	if !g.AcquireAdmission(context.Background()) {
		t.Error("AcquireAdmission after ReleaseAdmission failed")
	}
}
