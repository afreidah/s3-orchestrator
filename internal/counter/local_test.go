// -------------------------------------------------------------------------------
// LocalCounterBackend Tests
//
// Author: Alex Freidah
//
// Tests for the in-memory atomic counter backend. Validates charge and load,
// the take and settle round trip, bucket keying, unknown backend handling, and
// concurrent charges racing a take.
// -------------------------------------------------------------------------------

package counter

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

const (
	backendB1  = "b1"
	poolClassA = "class_a"
	workloadA  = "scrub"
	workloadB  = "client"
)

// oldBucket is a bucket well before any test's current one.
var oldBucket = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// loaded returns a backend's unflushed counters in every bucket as a fresh
// value, the way a caller that wants one reading uses Load.
func loaded(b Backend, name string) core.Usage {
	var u core.Usage
	b.Load(name, time.Time{}, &u)
	return u
}

// workloadsOf sums a batch's buckets per workload, for assertions about what
// was charged regardless of the hour it was charged in.
func workloadsOf(b core.UsageBatch) core.WorkloadUsage {
	out := make(core.WorkloadUsage)
	_ = b.Each(func(_ time.Time, workload string, u *core.Usage) error {
		w := out[workload]
		w.Add(*u)
		out[workload] = w
		return nil
	})
	return out
}

// totalOf sums every bucket and workload in a batch.
func totalOf(b core.UsageBatch) core.Usage {
	return workloadsOf(b).Total()
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

	cb.Charge(backendB1, oldBucket, Charge{APIRequests: 2, EgressBytes: 1024, IngressBytes: 2048, Pools: poolsOf(poolClassA, "class_b"), PoolN: 2})
	cb.Charge(backendB1, oldBucket, Charge{APIRequests: 3, Pools: poolsOf(poolClassA), PoolN: 3})

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

	cb.Charge(backendB1, oldBucket, Charge{APIRequests: 2, Pools: poolsOf(poolClassA, poolClassA), PoolN: 1})

	if got := loaded(cb, backendB1).Pools[poolClassA]; got != 2 {
		t.Errorf("class_a = %d, want 2", got)
	}
}

// TestLocalCounterBackend_IgnoresEmptyAndUnknown asserts an empty charge
// creates no counters and a charge for an unknown backend is dropped.
func TestLocalCounterBackend_IgnoresEmptyAndUnknown(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{backendB1})

	cb.Charge(backendB1, oldBucket, Charge{})
	cb.Charge(backendB1, oldBucket, Charge{Pools: poolsOf(poolClassA)})
	cb.Charge("unknown", oldBucket, Charge{APIRequests: 5})

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

	cb.Charge(backendB1, oldBucket, Charge{APIRequests: 1, Pools: poolsOf(poolClassA)})

	got := loaded(cb, backendB1)
	if got.APIRequests != 1 || got.Pools != nil {
		t.Errorf("usage = %+v, want one request and no pools", got)
	}
}

// TestLocalCounterBackend_SplitsByWorkload asserts charges are kept per
// workload, Load sums them, and Take returns one batch carrying each.
func TestLocalCounterBackend_SplitsByWorkload(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{backendB1})
	cb.Charge(backendB1, oldBucket, Charge{Workload: workloadA, APIRequests: 2, EgressBytes: 100})
	cb.Charge(backendB1, oldBucket, Charge{Workload: workloadB, APIRequests: 3, EgressBytes: 50})

	if got := loaded(cb, backendB1); got.APIRequests != 5 || got.EgressBytes != 150 {
		t.Errorf("Load = %+v, want both workloads summed", got)
	}
	batches := cb.Take(context.Background())
	if len(batches) != 1 {
		t.Fatalf("Take returned %d batches, want 1", len(batches))
	}
	w := workloadsOf(batches[0])
	if w[workloadA].EgressBytes != 100 || w[workloadB].EgressBytes != 50 {
		t.Errorf("workloads = %+v, want 100 for %s and 50 for %s", w, workloadA, workloadB)
	}
}

// TestLocalCounterBackend_TakeEmptiesTheCounters asserts Take hands over one
// batch per charged backend, with no ID, and leaves the counters at zero.
func TestLocalCounterBackend_TakeEmptiesTheCounters(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{backendB1, "b2"})
	cb.Charge(backendB1, oldBucket, Charge{APIRequests: 10, Pools: poolsOf(poolClassA), PoolN: 10})
	cb.Charge("b2", oldBucket, Charge{EgressBytes: 300})

	batches := cb.Take(context.Background())
	if len(batches) != 2 {
		t.Fatalf("Take returned %d batches, want 2", len(batches))
	}
	byBackend := map[string]core.UsageBatch{}
	for _, b := range batches {
		if b.ID != "" {
			t.Errorf("batch %+v, want no ID", b)
		}
		byBackend[b.Backend] = b
	}
	if u := totalOf(byBackend[backendB1]); u.APIRequests != 10 || u.Pools[poolClassA] != 10 {
		t.Errorf("b1 batch = %+v, want 10 requests and 10 against class_a", u)
	}
	if u := totalOf(byBackend["b2"]); u.EgressBytes != 300 {
		t.Errorf("b2 batch = %+v, want 300 egress", u)
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
	cb.Charge(backendB1, oldBucket, Charge{APIRequests: 4, Pools: poolsOf(poolClassA), PoolN: 4})

	batches := cb.Take(ctx)
	cb.Settle(ctx, &batches[0], true)
	if got := cb.Take(ctx); len(got) != 0 {
		t.Fatalf("Take after an applied settle = %+v, want nothing", got)
	}

	cb.Charge(backendB1, oldBucket, Charge{APIRequests: 4, Pools: poolsOf(poolClassA), PoolN: 4})
	batches = cb.Take(ctx)
	cb.Settle(ctx, &batches[0], false)
	if got := loaded(cb, backendB1); got.APIRequests != 4 || got.Pools[poolClassA] != 4 {
		t.Errorf("Load after an unapplied settle = %+v, want the batch back", got)
	}
	again := cb.Take(ctx)
	if len(again) != 1 || totalOf(again[0]).APIRequests != 4 {
		t.Errorf("Take after an unapplied settle = %+v, want the batch again", again)
	}
}

// TestLocalCounterBackend_SettleKeepsTheBucket asserts a batch put back by
// Settle stays in its own bucket, outside a window that starts after it.
func TestLocalCounterBackend_SettleKeepsTheBucket(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cb := NewLocalCounterBackend([]string{backendB1})
	old := core.UsageBatch{Backend: backendB1, Buckets: core.BucketUsage{oldBucket: {workloadA: {
		UsageStat: core.UsageStat{APIRequests: 7},
		Pools:     core.PoolUsage{poolClassA: 7},
	}}}}

	cb.Settle(ctx, &old, false)

	var recent core.Usage
	cb.Load(backendB1, oldBucket.Add(time.Hour), &recent)
	if !recent.IsZero() {
		t.Errorf("window after %v = %+v, want zero", oldBucket, recent)
	}
	batches := cb.Take(ctx)
	if len(batches) != 1 || batches[0].Buckets[oldBucket][workloadA].Pools[poolClassA] != 7 {
		t.Errorf("Take = %+v, want the batch back in its bucket", batches)
	}
}

// TestLocalCounterBackend_LoadSince asserts Load sums only buckets starting
// at or after since, so a check reads the current cycle alone.
func TestLocalCounterBackend_LoadSince(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{backendB1})
	cycleStart := oldBucket.Add(24 * time.Hour)
	cb.Charge(backendB1, cycleStart.Add(-time.Hour), Charge{APIRequests: 5})
	cb.Charge(backendB1, cycleStart, Charge{APIRequests: 2})
	cb.Charge(backendB1, cycleStart.Add(3*time.Hour), Charge{APIRequests: 1})

	var u core.Usage
	cb.Load(backendB1, cycleStart, &u)
	if u.APIRequests != 3 {
		t.Errorf("APIRequests since %v = %d, want 3", cycleStart, u.APIRequests)
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
	cb.Charge(backendB1, oldBucket, Charge{APIRequests: 1})
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
			cb.Charge(backendB1, oldBucket, Charge{APIRequests: 1})
		}
	})
	var total int64
	for range 50 {
		for _, b := range cb.Take(ctx) {
			total += totalOf(b).APIRequests
		}
	}
	wg.Wait()
	for _, b := range cb.Take(ctx) {
		total += totalOf(b).APIRequests
	}

	if total != iterations {
		t.Errorf("total taken = %d, want %d", total, iterations)
	}
}
