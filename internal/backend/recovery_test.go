// -------------------------------------------------------------------------------
// Backend Recovery Tests
//
// Author: Alex Freidah
//
// Covers what a backend adds to a breaker's recovery: each check is admitted
// only while the usage budget has room and is charged as the operation it
// makes, and a call breaker's check retries that call. The backoff schedule is
// the breaker package's and is tested there.
// -------------------------------------------------------------------------------

package backend

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// fakeUsage admits health checks unless refuse is set and records charges.
type fakeUsage struct {
	mu      sync.Mutex
	refuse  bool
	charged []s3op.Operation
}

// WithinLimits reports whether the fake is admitting calls.
func (f *fakeUsage) WithinLimits(string, s3op.Operation, int64, int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.refuse
}

// Record logs the charged operation.
func (f *fakeUsage) Record(_ context.Context, _ string, op s3op.Operation, _, _ int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.charged = append(f.charged, op)
}

// TestProbeRecovery_AdmitsAndCharges verifies a check is admitted and charged
// as the probe's operation while the budget has room, and refused without a
// charge once it does not.
func TestProbeRecovery_AdmitsAndCharges(t *testing.T) {
	t.Parallel()
	cb := newTestCBBackend(newMockBackend(), 1, time.Minute)
	usage := &fakeUsage{}
	r := probeFor(t, cb, s3op.HeadBucket).Recovery(cb.Name(), usage)

	if !r.Admit(t.Context()) {
		t.Fatal("check refused while the budget has room")
	}
	usage.refuse = true
	if r.Admit(t.Context()) {
		t.Error("check admitted over the budget")
	}
	if len(usage.charged) != 1 || usage.charged[0] != s3op.HeadBucket {
		t.Errorf("charged %v, want one HeadBucket", usage.charged)
	}
	if r.MaxBackoff != maxRecoveryBackoff {
		t.Errorf("MaxBackoff = %v, want %v", r.MaxBackoff, maxRecoveryBackoff)
	}
}

// TestProbeRecovery_CallProbe verifies a call breaker's check retries that
// call, and that recovering its breaker restores only that call.
func TestProbeRecovery_CallProbe(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	mock.getErr = &httpError{code: 403}
	cb := newTestCBBackend(mock, 1, time.Minute)
	_, _ = cb.GetObject(context.Background(), "key", "")
	probe := probeFor(t, cb, s3op.GetObject)
	r := probe.Recovery(cb.Name(), &fakeUsage{})

	if err := r.Check(t.Context()); err == nil {
		t.Fatal("GET check passed while the backend refuses GET")
	}
	mock.mu.Lock()
	mock.getErr = &httpError{code: 404}
	mock.mu.Unlock()
	if err := r.Check(t.Context()); err != nil {
		t.Fatalf("GET check = %v once the backend answers it", err)
	}
	probe.Breaker.Recover()
	if cb.Served() != core.AllCalls {
		t.Errorf("Served = %s, want every call once GET recovers", cb.Served())
	}
}
