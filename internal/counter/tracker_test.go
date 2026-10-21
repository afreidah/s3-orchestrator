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
	"maps"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
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

// spent seeds a period already partly consumed for one backend, keeping the
// baselines already set for others. The request total and the wildcard pool's
// share of it are set together, as the collector loads them.
func spent(tracker *UsageTracker, name string, api, egress, ingress int64) {
	u := core.Usage{UsageStat: core.UsageStat{APIRequests: api, EgressBytes: egress, IngressBytes: ingress}}
	if api > 0 {
		u.Pools = core.PoolUsage{core.PoolAll: api}
	}
	baselines := maps.Clone(*tracker.baseline.Load())
	baselines[name] = u
	tracker.SetBaselines(baselines)
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
	tracker.Record(t.Context(), "b1", s3op.GetObject, 150, 0) // unflushed egress pushes to 850/1000 = 85%

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
	tracker.RecordN(t.Context(), "b1", s3op.GetObject, 40) // baseline 50 + unflushed 40 = 90 effective

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
	tracker.SetBaselines(map[string]core.Usage{"b1": {
		UsageStat: core.UsageStat{APIRequests: 10},
		Pools:     core.PoolUsage{"class_a": 10},
	}})

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
	tracker.RecordN(t.Context(), "b1", s3op.ListObjects, 5)

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

	tracker.RecordN(t.Context(), "b1", s3op.GetObject, 0)
	tracker.RecordN(t.Context(), "b1", s3op.GetObject, -5)

	if got := loaded(backend, "b1"); !got.IsZero() {
		t.Errorf("counters = %+v, want all zero for empty charges", got)
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
	tracker.RecordN(t.Context(), "b1", s3op.GetObject, 1000)

	if !tracker.WithinLimits("b1", reads(1), 0, 0) {
		t.Error("a pool with no ceiling must never refuse")
	}
	if got := loaded(backend, "b1").Pools["watched"]; got != 1000 {
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
	tracker.RecordN(t.Context(), "b1", s3op.PutObject, 1)

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
	tracker.RecordN(t.Context(), "b1", s3op.PutObject, 1) // the only metered call the budget allows

	if tracker.WithinLimits("b1", []s3op.Operation{s3op.PutObject}, 0, 0) {
		t.Error("the budget is spent; a metered write must be refused")
	}
	if !tracker.WithinLimits("b1", []s3op.Operation{s3op.DeleteObject}, 0, 0) {
		t.Error("a delete the provider does not bill must never be refused")
	}

	// Recorded even though unbilled: not charging an operation is not a reason
	// to stop reporting that it happened.
	tracker.Record(t.Context(), "b1", s3op.DeleteObject, 0, 0)
	if got := loaded(backend, "b1").APIRequests; got != 2 {
		t.Errorf("api_requests = %d, want 2 (the write and the unmetered delete)", got)
	}
	if got := loaded(backend, "b1").Pools[core.PoolAll]; got != 1 {
		t.Errorf("pool count = %d, want 1; an unmetered delete must charge no pool", got)
	}
}

// TestRecord_ChargesCtxWorkload asserts a charge lands under the workload ctx
// carries, and under unknown when it carries none.
func TestRecord_ChargesCtxWorkload(t *testing.T) {
	t.Parallel()
	backend := NewLocalCounterBackend([]string{"b1"})
	tracker := NewUsageTracker(backend, nil)
	tracker.Record(audit.WithWorkload(t.Context(), audit.WorkloadScrub), "b1", s3op.GetObject, 10, 0)
	tracker.Record(t.Context(), "b1", s3op.GetObject, 5, 0)

	batches := backend.Take(t.Context())
	if len(batches) != 1 {
		t.Fatalf("Take returned %d batches, want 1", len(batches))
	}
	w := batches[0].Workloads
	if w[string(audit.WorkloadScrub)].EgressBytes != 10 || w[string(audit.WorkloadUnknown)].EgressBytes != 5 {
		t.Errorf("workloads = %+v, want 10 for scrub and 5 for unknown", w)
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
// SetBaselines
// -------------------------------------------------------------------------

// TestSetBaselines_ReplacesEveryBackend asserts a backend missing from the new
// map starts at zero, which is how a new period with no rows clears it.
func TestSetBaselines_ReplacesEveryBackend(t *testing.T) {
	t.Parallel()
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1", "b2"}), map[string]core.UsageLimits{
		"b1": capped(t, 1000, 0, 0),
		"b2": capped(t, 1000, 0, 0),
	})
	spent(tracker, "b1", 1000, 0, 0)
	spent(tracker, "b2", 1000, 0, 0)

	tracker.SetBaselines(map[string]core.Usage{"b2": {Pools: core.PoolUsage{core.PoolAll: 1000}}})

	if !tracker.WithinLimits("b1", reads(999), 0, 0) {
		t.Error("b1 is absent from the new baselines and should start at zero")
	}
	if tracker.WithinLimits("b2", reads(1), 0, 0) {
		t.Error("b2 is still spent in the new baselines")
	}

	tracker.SetBaselines(nil)
	if !tracker.WithinLimits("b2", reads(999), 0, 0) {
		t.Error("nil baselines should clear every backend")
	}
}

// TestNearLimit_PoolReachesThreshold asserts one tight pool is enough to want
// a shorter flush interval, even when the byte dimensions have room.
func TestNearLimit_PoolReachesThreshold(t *testing.T) {
	t.Parallel()
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), map[string]core.UsageLimits{
		"b1": pooled(t, []core.PoolSpec{
			{Name: "writes", Operations: []string{string(s3op.PutObject)}, Limit: 10},
			{Name: "reads", Operations: []string{string(s3op.GetObject)}, Limit: 1000},
		}),
	})
	tracker.RecordN(t.Context(), "b1", s3op.PutObject, 9)

	if !tracker.NearLimit(0.8) {
		t.Error("the write pool is at 90% and should count as near its limit")
	}
}

// -------------------------------------------------------------------------
// FlushUsage
// -------------------------------------------------------------------------

// TestFlushUsage_AppliesAndSettles asserts one batch carries the totals and
// pools together, and a batch the store applied leaves nothing behind.
func TestFlushUsage_AppliesAndSettles(t *testing.T) {
	t.Parallel()
	backend := NewLocalCounterBackend([]string{"b1"})
	tracker := NewUsageTracker(backend, map[string]core.UsageLimits{"b1": capped(t, 1000, 0, 0)})
	tracker.RecordN(t.Context(), "b1", s3op.GetObject, 10)
	tracker.Record(t.Context(), "b1", s3op.GetObject, 100, 50)

	flusher := &recordingFlusher{}
	if err := tracker.FlushUsage(context.Background(), flusher); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(flusher.applied) != 1 {
		t.Fatalf("applied %d batches, want 1", len(flusher.applied))
	}
	b := flusher.applied[0]
	if b.ID != "" || b.Backend != "b1" || b.Period != CurrentPeriod() {
		t.Errorf("batch = %+v, want b1 in the current period with no ID", b)
	}
	if u := b.Workloads.Total(); u.APIRequests != 11 || u.EgressBytes != 100 || u.IngressBytes != 50 || u.Pools[core.PoolAll] != 11 {
		t.Errorf("batch usage = %+v, want 11/100/50 and 11 against %q", u, core.PoolAll)
	}
	if got := loaded(backend, "b1"); !got.IsZero() {
		t.Errorf("counters after flush = %+v, want zero", got)
	}
}

// TestFlushUsage_FailedApplyKeepsTheBatch asserts a batch the store refused
// goes back to the counters and is carried by the next flush.
func TestFlushUsage_FailedApplyKeepsTheBatch(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{"b1"})
	tracker := NewUsageTracker(cb, map[string]core.UsageLimits{"b1": capped(t, 1000, 0, 0)})
	tracker.RecordN(t.Context(), "b1", s3op.GetObject, 10)

	if err := tracker.FlushUsage(context.Background(), &recordingFlusher{err: errDBDown}); !errors.Is(err, errDBDown) {
		t.Fatalf("FlushUsage error = %v, want %v", err, errDBDown)
	}
	got := loaded(cb, "b1")
	if got.APIRequests != 10 || got.Pools[core.PoolAll] != 10 {
		t.Errorf("counters after a failed flush = %+v, want 10 requests and 10 against the pool", got)
	}

	flusher := &recordingFlusher{}
	if err := tracker.FlushUsage(context.Background(), flusher); err != nil {
		t.Fatalf("second flush: %v", err)
	}
	if len(flusher.applied) != 1 || flusher.applied[0].Workloads.Total().APIRequests != 10 {
		t.Errorf("second flush applied %+v, want the kept batch of 10", flusher.applied)
	}
}

// TestFlushUsage_SharedBatches asserts a shared backend's staged batches are
// flushed beside the local ones and settled with the store's answer.
func TestFlushUsage_SharedBatches(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		err     error
		applied bool
	}{
		{name: "applied", applied: true},
		{name: "failed", err: errDBDown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backend := newFakeShared()
			tracker := NewUsageTracker(backend, nil)
			tracker.RecordN(t.Context(), "b1", s3op.GetObject, 3)

			flusher := &recordingFlusher{err: tc.err}
			if err := tracker.FlushUsage(context.Background(), flusher); !errors.Is(err, tc.err) {
				t.Fatalf("FlushUsage error = %v, want %v", err, tc.err)
			}
			if len(flusher.applied) != 2 {
				t.Fatalf("applied %d batches, want the local and the staged one", len(flusher.applied))
			}
			if got, ok := backend.settled[stagedBatchID]; !ok || got != tc.applied {
				t.Errorf("staged batch settled = %v (seen %v), want %v", got, ok, tc.applied)
			}
			if kept := loaded(backend, "b1").APIRequests; tc.applied == (kept != 0) {
				t.Errorf("local requests after flush = %d, applied = %v", kept, tc.applied)
			}
		})
	}
}

// TestFlushUsage_PlainBackendSkipsShared asserts staged batches are taken only
// from a backend that is a SharedBackend.
func TestFlushUsage_PlainBackendSkipsShared(t *testing.T) {
	t.Parallel()
	tracker := NewUsageTracker(plainBackend{newFakeShared()}, nil)
	tracker.RecordN(t.Context(), "b1", s3op.GetObject, 3)

	flusher := &recordingFlusher{}
	if err := tracker.FlushUsage(context.Background(), flusher); err != nil {
		t.Fatalf("FlushUsage: %v", err)
	}
	if len(flusher.applied) != 1 || flusher.applied[0].ID != "" {
		t.Errorf("applied %+v, want only the local batch", flusher.applied)
	}
}

// TestCurrentPeriod_RefreshesAnExpiredCache asserts a cached month past its
// end is replaced rather than returned.
func TestCurrentPeriod_RefreshesAnExpiredCache(t *testing.T) {
	t.Parallel()
	currentMonth.Store(&monthCache{period: "1999-01", until: time.Unix(0, 0)})

	want := monthStart(time.Now()).Format("2006-01")
	if got := CurrentPeriod(); got != want {
		t.Errorf("CurrentPeriod() = %q, want %q", got, want)
	}
	if c := currentMonth.Load(); c.period != want || !c.until.After(time.Now()) {
		t.Errorf("cache = %+v, want %q until a future instant", c, want)
	}
}

// TestMonthStart asserts the first instant of the month is taken in UTC,
// whatever zone the input is in.
func TestMonthStart(t *testing.T) {
	t.Parallel()
	east := time.FixedZone("east", 5*3600)
	for _, tc := range []struct {
		in   time.Time
		want string
	}{
		{in: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC), want: "2026-10-01T00:00:00Z"},
		{in: time.Date(2026, 11, 1, 2, 0, 0, 0, east), want: "2026-10-01T00:00:00Z"},
		{in: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), want: "2027-01-01T00:00:00Z"},
	} {
		if got := monthStart(tc.in).Format(time.RFC3339); got != tc.want {
			t.Errorf("monthStart(%v) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// TestFlushPeriods_CurrentThenPrevious asserts the flush covers the current
// month and the one before it.
func TestFlushPeriods_CurrentThenPrevious(t *testing.T) {
	t.Parallel()
	periods := flushPeriods()
	if len(periods) != 2 || periods[0] != CurrentPeriod() {
		t.Fatalf("flushPeriods() = %v, want the current period first", periods)
	}
	if periods[1] >= periods[0] {
		t.Errorf("previous period %q should sort before %q", periods[1], periods[0])
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

// stagedBatchID is the flush ID of the batch fakeShared stages.
const stagedBatchID = "flush-1"

// errDBDown is the store failure the flush tests inject.
var errDBDown = errors.New("db down")

// recordingFlusher records every batch it is handed and answers each with err.
type recordingFlusher struct {
	applied []core.UsageBatch
	err     error
}

// ApplyUsageBatch records the batch and returns the configured error, so the
// tracker's settle path sees the store's answer.
func (f *recordingFlusher) ApplyUsageBatch(_ context.Context, b *core.UsageBatch) error {
	f.applied = append(f.applied, *b)
	return f.err
}

// fakeShared is a SharedBackend over local counters with one staged batch. It
// records how each staged batch was settled.
type fakeShared struct {
	*LocalCounterBackend
	settled map[string]bool
}

// newFakeShared builds a fakeShared tracking backend b1.
func newFakeShared() *fakeShared {
	return &fakeShared{LocalCounterBackend: NewLocalCounterBackend([]string{"b1"}), settled: map[string]bool{}}
}

// TakeShared returns the one staged batch, as a shared store would after a
// stage.
func (f *fakeShared) TakeShared(_ context.Context) []core.UsageBatch {
	return []core.UsageBatch{{ID: stagedBatchID, Backend: "b1", Period: CurrentPeriod(),
		Workloads: core.WorkloadUsage{workloadA: {UsageStat: core.UsageStat{APIRequests: 7}}}}}
}

// Settle records a staged batch's outcome and hands a local batch to the
// local counters.
func (f *fakeShared) Settle(ctx context.Context, b *core.UsageBatch, applied bool) {
	if b.ID != "" {
		f.settled[b.ID] = applied
		return
	}
	f.LocalCounterBackend.Settle(ctx, b, applied)
}

// plainBackend hides a backend's TakeShared, leaving only the Backend
// interface.
type plainBackend struct {
	Backend
}
