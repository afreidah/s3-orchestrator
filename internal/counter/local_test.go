// -------------------------------------------------------------------------------
// LocalCounterBackend Tests
//
// Author: Alex Freidah
//
// Tests for the in-memory atomic counter backend. Validates charge and load,
// the take and settle round trip, period keying, unknown backend handling, and
// concurrent charges racing a take.
// -------------------------------------------------------------------------------

package counter

import (
	"context"
	"sync"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

const (
	backendB1  = "b1"
	poolClassA = "class_a"
	oldPeriod  = "2020-01"
)

// loaded returns a backend's unflushed counters as a fresh value, the way a
// caller that wants one reading uses Load.
func loaded(b Backend, name string) core.Usage {
	var u core.Usage
	b.Load(name, &u)
	return u
}

// poolsOf builds request pools from names, for a Charge.
func poolsOf(names ...string) []core.RequestPool {
	pools := make([]core.RequestPool, len(names))
	for i, n := range names {
		pools[i] = core.RequestPool{Name: n}
	}
	return pools
}

// TestLocalCounterBackend_ChargeAndLoad asserts charges accumulate into the
// current period's totals and pools.
func TestLocalCounterBackend_ChargeAndLoad(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{backendB1})

	cb.Charge(backendB1, Charge{APIRequests: 2, EgressBytes: 1024, IngressBytes: 2048, Pools: poolsOf(poolClassA, "class_b"), PoolN: 2})
	cb.Charge(backendB1, Charge{APIRequests: 3, Pools: poolsOf(poolClassA), PoolN: 3})

	got := loaded(cb, backendB1)
	if got.APIRequests != 5 || got.EgressBytes != 1024 || got.IngressBytes != 2048 {
		t.Errorf("totals = %+v, want 5/1024/2048", got.UsageStat)
	}
	if got.Pools[poolClassA] != 5 || got.Pools["class_b"] != 2 {
		t.Errorf("pools = %v, want class_a 5 and class_b 2", got.Pools)
	}
}

// TestLocalCounterBackend_PoolListedTwiceIsChargedTwice asserts a pool named
// by two operations of one charge counts both.
func TestLocalCounterBackend_PoolListedTwiceIsChargedTwice(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{backendB1})

	cb.Charge(backendB1, Charge{APIRequests: 2, Pools: poolsOf(poolClassA, poolClassA), PoolN: 1})

	if got := loaded(cb, backendB1).Pools[poolClassA]; got != 2 {
		t.Errorf("class_a = %d, want 2", got)
	}
}

// TestLocalCounterBackend_IgnoresEmptyAndUnknown asserts an empty charge
// creates no counters and a charge for an unknown backend is dropped.
func TestLocalCounterBackend_IgnoresEmptyAndUnknown(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{backendB1})

	cb.Charge(backendB1, Charge{})
	cb.Charge(backendB1, Charge{Pools: poolsOf(poolClassA)})
	cb.Charge("unknown", Charge{APIRequests: 5})

	if got := loaded(cb, "unknown"); !got.IsZero() {
		t.Errorf("unknown backend = %+v, want zero", got)
	}
	if got := cb.Take(context.Background()); len(got) != 0 {
		t.Errorf("Take = %+v, want no batches", got)
	}
}

// TestLocalCounterBackend_ZeroPoolNChargesNoPool asserts the totals still
// count when the charge carries pools but no pool count.
func TestLocalCounterBackend_ZeroPoolNChargesNoPool(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{backendB1})

	cb.Charge(backendB1, Charge{APIRequests: 1, Pools: poolsOf(poolClassA)})

	got := loaded(cb, backendB1)
	if got.APIRequests != 1 || got.Pools != nil {
		t.Errorf("usage = %+v, want one request and no pools", got)
	}
}

// TestLocalCounterBackend_TakeEmptiesTheCounters asserts Take hands over one
// batch per charged backend, with no ID, and leaves the counters at zero.
func TestLocalCounterBackend_TakeEmptiesTheCounters(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{backendB1, "b2"})
	cb.Charge(backendB1, Charge{APIRequests: 10, Pools: poolsOf(poolClassA), PoolN: 10})
	cb.Charge("b2", Charge{EgressBytes: 300})

	batches := cb.Take(context.Background())
	if len(batches) != 2 {
		t.Fatalf("Take returned %d batches, want 2", len(batches))
	}
	byBackend := map[string]core.UsageBatch{}
	for _, b := range batches {
		if b.ID != "" || b.Period != CurrentPeriod() {
			t.Errorf("batch %+v, want no ID and the current period", b)
		}
		byBackend[b.Backend] = b
	}
	if b := byBackend[backendB1]; b.APIRequests != 10 || b.Pools[poolClassA] != 10 {
		t.Errorf("b1 batch = %+v, want 10 requests and 10 against class_a", b.Usage)
	}
	if b := byBackend["b2"]; b.EgressBytes != 300 {
		t.Errorf("b2 batch = %+v, want 300 egress", b.Usage)
	}
	if got := loaded(cb, backendB1); !got.IsZero() {
		t.Errorf("b1 after Take = %+v, want zero", got)
	}
}

// TestLocalCounterBackend_SettleRoundTrip asserts an applied batch is gone
// and an unapplied one is carried by the next Take.
func TestLocalCounterBackend_SettleRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cb := NewLocalCounterBackend([]string{backendB1})
	cb.Charge(backendB1, Charge{APIRequests: 4, Pools: poolsOf(poolClassA), PoolN: 4})

	batches := cb.Take(ctx)
	cb.Settle(ctx, &batches[0], true)
	if got := cb.Take(ctx); len(got) != 0 {
		t.Fatalf("Take after an applied settle = %+v, want nothing", got)
	}

	cb.Charge(backendB1, Charge{APIRequests: 4, Pools: poolsOf(poolClassA), PoolN: 4})
	batches = cb.Take(ctx)
	cb.Settle(ctx, &batches[0], false)
	if got := loaded(cb, backendB1); got.APIRequests != 4 || got.Pools[poolClassA] != 4 {
		t.Errorf("Load after an unapplied settle = %+v, want the batch back", got)
	}
	again := cb.Take(ctx)
	if len(again) != 1 || again[0].APIRequests != 4 {
		t.Errorf("Take after an unapplied settle = %+v, want the batch again", again)
	}
}

// TestLocalCounterBackend_SettleKeepsThePeriod asserts a batch put back by
// Settle stays in its own period rather than moving into the current one.
func TestLocalCounterBackend_SettleKeepsThePeriod(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cb := NewLocalCounterBackend([]string{backendB1})
	old := core.UsageBatch{Backend: backendB1, Period: oldPeriod, Usage: core.Usage{
		UsageStat: core.UsageStat{APIRequests: 7},
		Pools:     core.PoolUsage{poolClassA: 7},
	}}

	cb.Settle(ctx, &old, false)

	if got := loaded(cb, backendB1); !got.IsZero() {
		t.Errorf("current period = %+v, want zero; the batch belongs to %s", got, oldPeriod)
	}
	batches := cb.Take(ctx)
	if len(batches) != 1 || batches[0].Period != oldPeriod || batches[0].Pools[poolClassA] != 7 {
		t.Errorf("Take = %+v, want the batch back in %s", batches, oldPeriod)
	}
}

// TestLocalCounterBackend_Backends asserts the configured names are returned,
// and a nil list tracks nothing.
func TestLocalCounterBackend_Backends(t *testing.T) {
	t.Parallel()
	if got := NewLocalCounterBackend([]string{"alpha", "beta"}).Backends(); len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Errorf("Backends() = %v, want [alpha beta]", got)
	}

	cb := NewLocalCounterBackend(nil)
	cb.Charge(backendB1, Charge{APIRequests: 1})
	if got := loaded(cb, backendB1); !got.IsZero() || len(cb.Backends()) != 0 {
		t.Errorf("nil backends: Load = %+v, Backends = %v, want nothing tracked", got, cb.Backends())
	}
}

// TestLocalCounterBackend_ChargesRacingTake asserts concurrent charges are
// never lost between a take and the counters that follow it.
func TestLocalCounterBackend_ChargesRacingTake(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cb := NewLocalCounterBackend([]string{backendB1})
	const iterations = 10000

	var wg sync.WaitGroup
	wg.Go(func() {
		for range iterations {
			cb.Charge(backendB1, Charge{APIRequests: 1})
		}
	})
	var total int64
	for range 50 {
		for _, b := range cb.Take(ctx) {
			total += b.APIRequests
		}
	}
	wg.Wait()
	for _, b := range cb.Take(ctx) {
		total += b.APIRequests
	}

	if total != iterations {
		t.Errorf("total taken = %d, want %d", total, iterations)
	}
}
