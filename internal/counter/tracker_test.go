// -------------------------------------------------------------------------------
// Usage Tracker Tests
//
// Author: Alex Freidah
//
// Tests for the atomic usage counter and periodic flush mechanism. Validates
// near-limit threshold calculations, pooled request admission, and counter
// accumulation behavior.
//
// Most cases use a single wildcard pool, which is what a bare
// api_request_limit desugars into and therefore the shape every pre-pool
// deployment runs. The pool-specific cases below cover what that shape cannot
// express: separate allowances per operation class, and operations charged to
// no budget at all.
// -------------------------------------------------------------------------------

package counter

import (
	"context"
	"errors"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// Test helpers
// -------------------------------------------------------------------------

// capped builds one backend's compiled limits from a bare monthly request cap
// plus byte caps, the shape api_request_limit desugars into.
func capped(t *testing.T, api, egress, ingress int64) core.UsageLimits {
	t.Helper()
	lim, err := core.NewUsageLimits(egress, ingress, core.SingleRequestPool(api), nil)
	if err != nil {
		t.Fatalf("build limits: %v", err)
	}
	return lim
}

// pooled builds limits from explicit pool specs and unmetered operations.
func pooled(t *testing.T, specs []core.PoolSpec, unmetered ...s3op.Operation) core.UsageLimits {
	t.Helper()
	lim, err := core.NewUsageLimits(0, 0, specs, unmetered)
	if err != nil {
		t.Fatalf("build limits: %v", err)
	}
	return lim
}

// spent seeds a period already partly consumed. Both halves of the baseline
// are set together because that is how the collector loads them: the request
// total for reporting, and the wildcard pool's share of it for admission.
func spent(tracker *UsageTracker, name string, api, egress, ingress int64) {
	stat := core.UsageStat{APIRequests: api, EgressBytes: egress, IngressBytes: ingress}
	var pools core.PoolUsage
	if api > 0 {
		pools = core.PoolUsage{core.PoolAll: api}
	}
	tracker.SetBaseline(name, stat, pools)
}

// reads is n GetObject operations, for the cases that care about how many
// calls are proposed rather than which.
func reads(n int) []s3op.Operation {
	ops := make([]s3op.Operation, n)
	for i := range ops {
		ops[i] = s3op.GetObject
	}
	return ops
}

// -------------------------------------------------------------------------
// NearLimit
// -------------------------------------------------------------------------

// TestNewUsageTracker_NilLimits verifies the new usage tracker nil limits path by exercising tracker.NearLimit.
func TestNewUsageTracker_NilLimits(t *testing.T) {
	t.Parallel()
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), nil)
	// Should not panic; nil limits treated as empty map.
	if tracker.NearLimit(0.8) {
		t.Error("nil limits should never be near limit")
	}
}

// TestNearLimit_BelowThreshold verifies the near limit below threshold path by exercising tracker.SetBaseline, tracker.NearLimit.
func TestNearLimit_BelowThreshold(t *testing.T) {
	t.Parallel()
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), map[string]core.UsageLimits{
		"b1": capped(t, 1000, 1000, 0),
	})
	spent(tracker, "b1", 100, 100, 0)

	if tracker.NearLimit(0.8) {
		t.Error("should not be near limit at 10% usage")
	}
}

// TestNearLimit_AboveThreshold verifies the near limit above threshold path by exercising tracker.SetBaseline, tracker.NearLimit.
func TestNearLimit_AboveThreshold(t *testing.T) {
	t.Parallel()
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), map[string]core.UsageLimits{
		"b1": capped(t, 1000, 0, 0),
	})
	spent(tracker, "b1", 850, 0, 0)

	if !tracker.NearLimit(0.8) {
		t.Error("should be near limit at 85% usage")
	}
}

// TestNearLimit_NoLimitsConfigured verifies the near limit no limits configured path by exercising tracker.SetBaseline, tracker.NearLimit.
func TestNearLimit_NoLimitsConfigured(t *testing.T) {
	t.Parallel()
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), map[string]core.UsageLimits{
		"b1": {}, // no pools, no byte caps = unlimited
	})
	spent(tracker, "b1", 999999, 0, 0)

	if tracker.NearLimit(0.8) {
		t.Error("should return false when no limits are configured")
	}
}

// TestNearLimit_ZeroLimitDimension verifies the near limit zero limit dimension path by exercising tracker.SetBaseline, tracker.NearLimit.
func TestNearLimit_ZeroLimitDimension(t *testing.T) {
	t.Parallel()
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), map[string]core.UsageLimits{
		"b1": capped(t, 0, 1000, 0), // requests unlimited, egress limited
	})
	spent(tracker, "b1", 999999, 100, 0)

	if tracker.NearLimit(0.8) {
		t.Error("should ignore unlimited request dimension; egress at 10% is not near limit")
	}
}

// TestNearLimit_UnflushedCounters verifies the near limit unflushed counters path by exercising tracker.SetBaseline, tracker.Record, tracker.NearLimit.
func TestNearLimit_UnflushedCounters(t *testing.T) {
	t.Parallel()
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), map[string]core.UsageLimits{
		"b1": capped(t, 0, 1000, 0),
	})
	spent(tracker, "b1", 0, 700, 0)
	tracker.Record("b1", s3op.GetObject, 150, 0) // unflushed egress pushes to 850/1000 = 85%

	if !tracker.NearLimit(0.8) {
		t.Error("should be near limit when baseline + unflushed exceeds threshold")
	}
}

// TestNearLimit_MultipleBackends verifies the near limit multiple backends path by exercising tracker.SetBaseline, tracker.NearLimit.
func TestNearLimit_MultipleBackends(t *testing.T) {
	t.Parallel()
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1", "b2"}), map[string]core.UsageLimits{
		"b1": capped(t, 1000, 0, 0),
		"b2": capped(t, 1000, 0, 0),
	})
	spent(tracker, "b1", 100, 0, 0) // 10% - fine
	spent(tracker, "b2", 900, 0, 0) // 90% - near limit

	if !tracker.NearLimit(0.8) {
		t.Error("should return true when any backend is near limit")
	}
}

// -------------------------------------------------------------------------
// WithinLimits
// -------------------------------------------------------------------------

// TestWithinLimits_AllWithinLimits verifies the within limits all within limits path by exercising tracker.WithinLimits.
func TestWithinLimits_AllWithinLimits(t *testing.T) {
	t.Parallel()
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), map[string]core.UsageLimits{
		"b1": capped(t, 1000, 1000, 1000),
	})
	if !tracker.WithinLimits("b1", reads(1), 1, 1) {
		t.Error("should be within limits at zero usage")
	}
}

// TestWithinLimits_APILimitExceeded verifies the within limits apilimit exceeded path by exercising tracker.SetBaseline, tracker.WithinLimits.
func TestWithinLimits_APILimitExceeded(t *testing.T) {
	t.Parallel()
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), map[string]core.UsageLimits{
		"b1": capped(t, 100, 0, 0),
	})
	spent(tracker, "b1", 99, 0, 0)

	if tracker.WithinLimits("b1", reads(2), 0, 0) {
		t.Error("should exceed request limit (99 + 2 > 100)")
	}
}

// TestWithinLimits_EgressLimitExceeded verifies the within limits egress limit exceeded path by exercising tracker.SetBaseline, tracker.WithinLimits.
func TestWithinLimits_EgressLimitExceeded(t *testing.T) {
	t.Parallel()
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), map[string]core.UsageLimits{
		"b1": capped(t, 0, 1000, 0),
	})
	spent(tracker, "b1", 0, 900, 0)

	if tracker.WithinLimits("b1", nil, 200, 0) {
		t.Error("should exceed egress limit (900 + 200 > 1000)")
	}
}

// TestWithinLimits_IngressLimitExceeded verifies the within limits ingress limit exceeded path by exercising tracker.SetBaseline, tracker.WithinLimits.
func TestWithinLimits_IngressLimitExceeded(t *testing.T) {
	t.Parallel()
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), map[string]core.UsageLimits{
		"b1": capped(t, 0, 0, 500),
	})
	spent(tracker, "b1", 0, 0, 400)

	if tracker.WithinLimits("b1", nil, 0, 200) {
		t.Error("should exceed ingress limit (400 + 200 > 500)")
	}
}

// TestWithinLimits_NoLimitsConfigured verifies the within limits no limits configured path by exercising tracker.WithinLimits.
func TestWithinLimits_NoLimitsConfigured(t *testing.T) {
	t.Parallel()
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), nil)

	if !tracker.WithinLimits("b1", reads(999), 999999, 999999) {
		t.Error("should be within limits when no limits configured")
	}
}

// TestWithinLimits_UnknownBackend verifies the within limits unknown backend path by exercising tracker.WithinLimits.
func TestWithinLimits_UnknownBackend(t *testing.T) {
	t.Parallel()
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), map[string]core.UsageLimits{
		"b1": capped(t, 100, 0, 0),
	})

	if !tracker.WithinLimits("unknown", reads(999), 0, 0) {
		t.Error("unknown backend should be within limits (no config)")
	}
}

// TestWithinLimits_IncludesUnflushedCounters verifies the within limits includes unflushed counters path by exercising tracker.SetBaseline, tracker.RecordN, tracker.WithinLimits.
func TestWithinLimits_IncludesUnflushedCounters(t *testing.T) {
	t.Parallel()
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), map[string]core.UsageLimits{
		"b1": capped(t, 100, 0, 0),
	})
	spent(tracker, "b1", 50, 0, 0)
	tracker.RecordN("b1", s3op.GetObject, 40) // baseline 50 + unflushed 40 = 90 effective

	if !tracker.WithinLimits("b1", reads(5), 0, 0) {
		t.Error("90 + 5 = 95 should be within limit of 100")
	}
	if tracker.WithinLimits("b1", reads(15), 0, 0) {
		t.Error("90 + 15 = 105 should exceed limit of 100")
	}
}

// -------------------------------------------------------------------------
// Pooled admission
// -------------------------------------------------------------------------

// TestWithinLimits_PoolsAreIndependent is the point of the feature: a backend
// out of write budget still serves reads, which a single counter cannot
// express. This is the gcp case - locked out of everything on a write budget
// while its far larger read allowance sat unused.
func TestWithinLimits_PoolsAreIndependent(t *testing.T) {
	t.Parallel()
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), map[string]core.UsageLimits{
		"b1": pooled(t, []core.PoolSpec{
			{Name: "class_a", Operations: []string{string(s3op.PutObject)}, Limit: 10},
			{Name: "class_b", Operations: []string{string(s3op.GetObject)}, Limit: 1000},
		}),
	})
	tracker.SetBaseline("b1", core.UsageStat{APIRequests: 10}, core.PoolUsage{"class_a": 10})

	if tracker.WithinLimits("b1", []s3op.Operation{s3op.PutObject}, 0, 0) {
		t.Error("the write pool is spent; a write must be refused")
	}
	if !tracker.WithinLimits("b1", []s3op.Operation{s3op.GetObject}, 0, 0) {
		t.Error("the read pool is untouched; a read must still be admitted")
	}
}

// TestWithinLimits_PoolsAreAdditive covers a sub-cap inside an aggregate one:
// an operation in two pools has to fit both, which is what lets an operator
// bound one operation without abandoning the overall ceiling.
func TestWithinLimits_PoolsAreAdditive(t *testing.T) {
	t.Parallel()
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), map[string]core.UsageLimits{
		"b1": pooled(t, []core.PoolSpec{
			{Name: "everything", Operations: []string{s3op.Wildcard}, Limit: 1000},
			{Name: "lists", Operations: []string{string(s3op.ListObjects)}, Limit: 5},
		}),
	})
	tracker.RecordN("b1", s3op.ListObjects, 5)

	if tracker.WithinLimits("b1", []s3op.Operation{s3op.ListObjects}, 0, 0) {
		t.Error("the list sub-cap is spent even though the aggregate has room")
	}
	if !tracker.WithinLimits("b1", []s3op.Operation{s3op.GetObject}, 0, 0) {
		t.Error("a read charges only the aggregate, which has room")
	}
}

// TestRecord_IgnoresEmptyCharges covers the guards on the charging path. A
// caller with nothing to charge must move no counter: charging an empty
// operation set as one call would inflate the request total on paths that
// batch, and a non-positive repeat count is a caller bug, not a credit.
func TestRecord_IgnoresEmptyCharges(t *testing.T) {
	t.Parallel()
	backend := NewLocalCounterBackend([]string{"b1"})
	tracker := NewUsageTracker(backend, map[string]core.UsageLimits{"b1": capped(t, 1000, 0, 0)})

	tracker.RecordAll("b1", nil, 100, 100)
	tracker.RecordN("b1", s3op.GetObject, 0)
	tracker.RecordN("b1", s3op.GetObject, -5)

	all := backend.LoadAll("b1")
	if all.APIRequests != 0 || all.EgressBytes != 0 || all.IngressBytes != 0 {
		t.Errorf("counters = %+v, want all zero for empty charges", all)
	}
	if got := backend.LoadPool("b1", core.PoolAll); got != 0 {
		t.Errorf("pool counter = %d, want 0", got)
	}
}

// TestWithinLimits_ZeroLimitPoolCountsWithoutRefusing covers the reporting-only
// budget: an operator can watch a class before deciding what to cap it at, and
// the pool accumulates without ever turning work away.
func TestWithinLimits_ZeroLimitPoolCountsWithoutRefusing(t *testing.T) {
	t.Parallel()
	backend := NewLocalCounterBackend([]string{"b1"})
	tracker := NewUsageTracker(backend, map[string]core.UsageLimits{
		"b1": pooled(t, []core.PoolSpec{
			{Name: "watched", Operations: []string{string(s3op.GetObject)}, Limit: 0},
		}),
	})
	tracker.RecordN("b1", s3op.GetObject, 1000)

	if !tracker.WithinLimits("b1", reads(1), 0, 0) {
		t.Error("a pool with no ceiling must never refuse")
	}
	if got := backend.LoadPool("b1", "watched"); got != 1000 {
		t.Errorf("pool count = %d, want 1000; an unbounded pool is still counted", got)
	}
	if tracker.NearLimit(0.8) {
		t.Error("a pool with no ceiling has no ratio and cannot be near one")
	}
}

// TestWithinLimits_UnchargedPoolIsNotConsulted pins that a spent budget only
// refuses the operations it covers. Without this the class split would collapse
// back into a single limit the moment one pool filled.
func TestWithinLimits_UnchargedPoolIsNotConsulted(t *testing.T) {
	t.Parallel()
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), map[string]core.UsageLimits{
		"b1": pooled(t, []core.PoolSpec{
			{Name: "writes", Operations: []string{string(s3op.PutObject)}, Limit: 1},
			{Name: "reads", Operations: []string{string(s3op.GetObject)}, Limit: 1000},
		}),
	})
	tracker.RecordN("b1", s3op.PutObject, 1)

	if !tracker.WithinLimits("b1", reads(1), 0, 0) {
		t.Error("a read was refused by a spent write budget it never charges")
	}
}

// TestWithinLimits_UnmeteredNeverRefuses covers the operations a provider does
// not bill. They are still recorded - the request happened - but no budget
// judges them, so a spent backend still accepts them.
func TestWithinLimits_UnmeteredNeverRefuses(t *testing.T) {
	t.Parallel()
	backend := NewLocalCounterBackend([]string{"b1"})
	tracker := NewUsageTracker(backend, map[string]core.UsageLimits{
		"b1": pooled(t,
			[]core.PoolSpec{{Name: core.PoolAll, Operations: []string{s3op.Wildcard}, Limit: 1}},
			s3op.DeleteObject),
	})
	tracker.RecordN("b1", s3op.PutObject, 1) // the only metered call the budget allows

	if tracker.WithinLimits("b1", []s3op.Operation{s3op.PutObject}, 0, 0) {
		t.Error("the budget is spent; a metered write must be refused")
	}
	if !tracker.WithinLimits("b1", []s3op.Operation{s3op.DeleteObject}, 0, 0) {
		t.Error("a delete the provider does not bill must never be refused")
	}

	// Recorded even though unbilled: not charging an operation is not a reason
	// to stop reporting that it happened.
	tracker.Record("b1", s3op.DeleteObject, 0, 0)
	if got := backend.LoadAll("b1").APIRequests; got != 2 {
		t.Errorf("api_requests = %d, want 2 (the write and the unmetered delete)", got)
	}
	if got := backend.LoadPool("b1", core.PoolAll); got != 1 {
		t.Errorf("pool count = %d, want 1; an unmetered delete must charge no pool", got)
	}
}

// TestRecordAll_ChargesEachOperationsPools covers the rewrite path: a pass that
// reads and writes one object charges the two pools its provider bills them to,
// rather than two of whichever pool a scalar count would have landed in.
func TestRecordAll_ChargesEachOperationsPools(t *testing.T) {
	t.Parallel()
	backend := NewLocalCounterBackend([]string{"b1"})
	tracker := NewUsageTracker(backend, map[string]core.UsageLimits{
		"b1": pooled(t, []core.PoolSpec{
			{Name: "class_a", Operations: []string{string(s3op.PutObject)}, Limit: 100},
			{Name: "class_b", Operations: []string{string(s3op.GetObject)}, Limit: 100},
		}),
	})
	tracker.RecordAll("b1", []s3op.Operation{s3op.GetObject, s3op.PutObject}, 10, 20)

	if got := backend.LoadPool("b1", "class_a"); got != 1 {
		t.Errorf("class_a = %d, want 1", got)
	}
	if got := backend.LoadPool("b1", "class_b"); got != 1 {
		t.Errorf("class_b = %d, want 1", got)
	}
	all := backend.LoadAll("b1")
	if all.APIRequests != 2 || all.EgressBytes != 10 || all.IngressBytes != 20 {
		t.Errorf("totals = %+v, want 2 requests / 10 egress / 20 ingress", all)
	}
}

// -------------------------------------------------------------------------
// BackendsWithinLimits
// -------------------------------------------------------------------------

// TestBackendsWithinLimits_FiltersCorrectly verifies the backends within limits filters correctly contract.
// Asserts that expected [b2], got.
func TestBackendsWithinLimits_FiltersCorrectly(t *testing.T) {
	t.Parallel()
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1", "b2"}), map[string]core.UsageLimits{
		"b1": capped(t, 10, 0, 0),
		"b2": capped(t, 1000, 0, 0),
	})
	spent(tracker, "b1", 10, 0, 0) // at limit

	eligible := tracker.BackendsWithinLimits([]string{"b1", "b2"}, reads(1), 0, 0)
	if len(eligible) != 1 || eligible[0] != "b2" {
		t.Errorf("expected [b2], got %v", eligible)
	}
}

// -------------------------------------------------------------------------
// UpdateLimits / GetLimits
// -------------------------------------------------------------------------

// TestUpdateLimits_GetLimits_RoundTrip verifies the update limits get limits round trip contract.
func TestUpdateLimits_GetLimits_RoundTrip(t *testing.T) {
	t.Parallel()
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), nil)
	tracker.UpdateLimits(map[string]core.UsageLimits{"b1": capped(t, 500, 0, 0)})

	pools := tracker.GetLimits()["b1"].Pools()
	if len(pools) != 1 || pools[0].Name != core.PoolAll || pools[0].Limit != 500 {
		t.Errorf("pools = %+v, want one %q pool of 500", pools, core.PoolAll)
	}
}

// TestGetLimits_ReturnsCopy verifies the get limits returns copy path by exercising tracker.GetLimits.
func TestGetLimits_ReturnsCopy(t *testing.T) {
	t.Parallel()
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), map[string]core.UsageLimits{
		"b1": capped(t, 100, 0, 0),
	})
	got := tracker.GetLimits()
	delete(got, "b1")

	// Original should be unaffected
	original := tracker.GetLimits()
	if _, ok := original["b1"]; !ok {
		t.Error("GetLimits should return a copy, not the original map")
	}
}

// -------------------------------------------------------------------------
// ResetBaselines
// -------------------------------------------------------------------------

// TestResetBaselines verifies the reset baselines path by exercising tracker.SetBaseline, tracker.ResetBaselines, tracker.WithinLimits.
func TestResetBaselines(t *testing.T) {
	t.Parallel()
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1", "b2"}), map[string]core.UsageLimits{
		"b1": capped(t, 1000, 0, 0),
		"b2": capped(t, 1000, 0, 0),
	})
	spent(tracker, "b1", 500, 0, 0)
	spent(tracker, "b2", 500, 0, 0)

	tracker.ResetBaselines([]string{"b1"})

	// Both halves of the baseline have to clear: a pool count left behind
	// would keep refusing work into the new period.
	if !tracker.WithinLimits("b1", reads(999), 0, 0) {
		t.Error("b1 baseline should be reset to zero")
	}
}

// -------------------------------------------------------------------------
// FlushUsage
// -------------------------------------------------------------------------

// TestFlushUsage_SwapsAndFlushes verifies the flush usage swaps and flushes contract.
func TestFlushUsage_SwapsAndFlushes(t *testing.T) {
	t.Parallel()
	backend := NewLocalCounterBackend([]string{"b1"})
	tracker := NewUsageTracker(backend, map[string]core.UsageLimits{"b1": capped(t, 1000, 0, 0)})
	tracker.RecordN("b1", s3op.GetObject, 10)
	tracker.Record("b1", s3op.GetObject, 100, 50)

	var flushedAPI, flushedEgress, flushedIngress int64
	var flushedPools core.PoolUsage
	mockFlusher := &mockUsageFlusher{
		fn: func(_, _ string, api, eg, ing int64) error {
			flushedAPI = api
			flushedEgress = eg
			flushedIngress = ing
			return nil
		},
		poolFn: func(_, _ string, deltas core.PoolUsage) error {
			flushedPools = deltas
			return nil
		},
	}

	err := tracker.FlushUsage(context.Background(), mockFlusher, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if flushedAPI != 11 || flushedEgress != 100 || flushedIngress != 50 {
		t.Errorf("flushed api=%d egress=%d ingress=%d, want 11/100/50", flushedAPI, flushedEgress, flushedIngress)
	}
	if flushedPools[core.PoolAll] != 11 {
		t.Errorf("flushed pools = %v, want 11 against %q", flushedPools, core.PoolAll)
	}

	// Counters should be zero after flush
	all := backend.LoadAll("b1")
	if all.APIRequests != 0 || all.EgressBytes != 0 || all.IngressBytes != 0 {
		t.Error("counters should be zero after flush")
	}
	if got := backend.LoadPool("b1", core.PoolAll); got != 0 {
		t.Errorf("pool counter = %d, want 0 after flush", got)
	}
}

// TestFlushUsage_SkipsBackendsInSkipMap verifies the flush usage skips backends in skip map path by exercising tracker.Record, tracker.FlushUsage, context.Background.
func TestFlushUsage_SkipsBackendsInSkipMap(t *testing.T) {
	t.Parallel()
	backend := NewLocalCounterBackend([]string{"b1"})
	tracker := NewUsageTracker(backend, nil)
	tracker.RecordN("b1", s3op.GetObject, 10)

	called := false
	mockFlusher := &mockUsageFlusher{fn: func(_, _ string, _, _, _ int64) error {
		called = true
		return nil
	}}

	_ = tracker.FlushUsage(context.Background(), mockFlusher, map[string]bool{"b1": true})
	if called {
		t.Error("should skip backends in skip map")
	}
}

// TestFlushUsage_RestoresOnError verifies the flush usage restores on error contract.
func TestFlushUsage_RestoresOnError(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{"b1"})
	tracker := NewUsageTracker(cb, nil)
	tracker.RecordN("b1", s3op.GetObject, 10)

	mockFlusher := &mockUsageFlusher{fn: func(_, _ string, _, _, _ int64) error {
		return errors.New("db down")
	}}

	err := tracker.FlushUsage(context.Background(), mockFlusher, nil)
	if err == nil {
		t.Fatal("expected error")
	}

	// Counter should be restored
	if cb.Load("b1", FieldAPIRequests) != 10 {
		t.Errorf("counter should be restored after flush error, got %d", cb.Load("b1", FieldAPIRequests))
	}
}

// TestFlushUsage_RestoresPoolsOnPoolError holds the halves apart: the totals
// landed, so restoring them too would double-count them on the next pass.
// Only the pool deltas that failed to write go back into the counter.
func TestFlushUsage_RestoresPoolsOnPoolError(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{"b1"})
	tracker := NewUsageTracker(cb, map[string]core.UsageLimits{"b1": capped(t, 1000, 0, 0)})
	tracker.RecordN("b1", s3op.GetObject, 10)

	mockFlusher := &mockUsageFlusher{
		fn:     func(_, _ string, _, _, _ int64) error { return nil },
		poolFn: func(_, _ string, _ core.PoolUsage) error { return errors.New("db down") },
	}

	if err := tracker.FlushUsage(context.Background(), mockFlusher, nil); err == nil {
		t.Fatal("expected error")
	}
	if got := cb.Load("b1", FieldAPIRequests); got != 0 {
		t.Errorf("api_requests = %d, want 0; the totals flushed successfully", got)
	}
	if got := cb.LoadPool("b1", core.PoolAll); got != 10 {
		t.Errorf("pool counter = %d, want 10 restored after the pool flush failed", got)
	}
}

// -------------------------------------------------------------------------
// Backend accessor
// -------------------------------------------------------------------------

// TestBackend_ReturnsUnderlyingBackend verifies the backend returns underlying backend path by exercising tracker.Backend.
func TestBackend_ReturnsUnderlyingBackend(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{"b1"})
	tracker := NewUsageTracker(cb, nil)
	if tracker.Backend() != cb {
		t.Error("Backend() should return the underlying counter backend")
	}
}

// -------------------------------------------------------------------------
// CurrentPeriod
// -------------------------------------------------------------------------

// TestCurrentPeriod_Format verifies the current period format contract.
// Asserts that CurrentPeriod() = , want YYYY-MM format.
func TestCurrentPeriod_Format(t *testing.T) {
	t.Parallel()
	p := CurrentPeriod()
	if len(p) != 7 || p[4] != '-' {
		t.Errorf("CurrentPeriod() = %q, want YYYY-MM format", p)
	}
}

// -------------------------------------------------------------------------
// Flusher double
// -------------------------------------------------------------------------

// mockUsageFlusher is a no-op flusher used by tests that exercise
// the tracker without standing up a real database. A nil poolFn accepts
// pool flushes silently, which is what the cases that only assert on the
// totals want.
type mockUsageFlusher struct {
	fn     func(name, period string, api, egress, ingress int64) error
	poolFn func(name, period string, deltas core.PoolUsage) error
}

// FlushUsageDeltas records the flush call for later assertion.
// Returns the test-configured error so the tracker's error path
// gets exercised.
func (m *mockUsageFlusher) FlushUsageDeltas(_ context.Context, name, period string, api, egress, ingress int64) error {
	return m.fn(name, period, api, egress, ingress)
}

// FlushPoolDeltas records the per-pool flush call.
func (m *mockUsageFlusher) FlushPoolDeltas(_ context.Context, name, period string, deltas core.PoolUsage) error {
	if m.poolFn == nil {
		return nil
	}
	return m.poolFn(name, period, deltas)
}
