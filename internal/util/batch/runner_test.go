// -------------------------------------------------------------------------------
// Batch Runner Tests
//
// Author: Alex Freidah
//
// Covers tallying sequentially and concurrently, a cancelled context
// dispatching nothing while Planned still reflects the input, Tally over
// gathered results, admission, and what makes a cycle reported or changed.
// -------------------------------------------------------------------------------

package batch

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
)

// TestRunner_Run_Tallies verifies sequential tallying of mixed outcomes.
func TestRunner_Run_Tallies(t *testing.T) {
	t.Parallel()
	items := []int{1, 2, 3, 4, 5}
	runner := Runner[int]{Concurrency: 1}
	sum := runner.Run(context.Background(), items, func(_ context.Context, n int) ItemResult {
		switch {
		case n%3 == 0: // 3 -> skipped
			return ItemResult{Outcome: ItemSkipped}
		case n%2 == 0: // 2, 4 -> failed
			return ItemResult{Outcome: ItemFailed}
		default: // 1, 5 -> succeeded
			return ItemResult{Outcome: ItemSucceeded}
		}
	})
	if sum.Planned != 5 || sum.Succeeded != 2 || sum.Failed != 2 || sum.Skipped != 1 || sum.Attempted != 4 {
		t.Errorf("summary = %+v, want planned=5 succeeded=2 failed=2 skipped=1 attempted=4", sum)
	}
	if sum.Outcome() != OutcomePartial {
		t.Errorf("Outcome() = %q, want %q", sum.Outcome(), OutcomePartial)
	}
}

// TestRunner_Run_Concurrent verifies the tally is correct (and race-free) when
// items run with concurrency > 1.
func TestRunner_Run_Concurrent(t *testing.T) {
	t.Parallel()
	const n = 200
	items := make([]int, n)
	for i := range items {
		items[i] = i
	}
	var ran atomic.Int64
	runner := Runner[int]{Concurrency: 8}
	sum := runner.Run(context.Background(), items, func(_ context.Context, _ int) ItemResult {
		ran.Add(1)
		return ItemResult{Outcome: ItemSucceeded}
	})
	if int(ran.Load()) != n || sum.Succeeded != n || sum.Planned != n {
		t.Errorf("ran=%d summary=%+v, want all %d succeeded", ran.Load(), sum, n)
	}
}

// TestRunner_Run_CancelledCtx verifies a pre-cancelled context dispatches
// nothing, leaving an empty summary.
func TestRunner_Run_CancelledCtx(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner := Runner[int]{Concurrency: 4}
	sum := runner.Run(ctx, []int{1, 2, 3}, func(_ context.Context, _ int) ItemResult {
		t.Error("fn must not run under a cancelled context")
		return ItemResult{Outcome: ItemSucceeded}
	})
	if sum.Attempted != 0 || sum.Succeeded != 0 {
		t.Errorf("summary = %+v, want nothing attempted", sum)
	}
	if sum.Planned != 3 {
		t.Errorf("Planned = %d, want 3 (it reflects the input, not what ran)", sum.Planned)
	}
}

// TestTally_CountsEachOutcome verifies Tally folds gathered results into the
// same counts a Runner reports and times them from start.
func TestTally_CountsEachOutcome(t *testing.T) {
	t.Parallel()
	start := time.Now().Add(-time.Second)
	sum := Tally([]ItemResult{
		{Outcome: ItemSucceeded},
		{Outcome: ItemFailed},
		{Outcome: ItemFailed},
		{},
	}, start)
	want := Summary{Planned: 4, Attempted: 3, Succeeded: 1, Failed: 2, Skipped: 1}
	got := sum
	got.Duration = 0
	if !reflect.DeepEqual(got, want) {
		t.Errorf("summary = %+v, want %+v", got, want)
	}
	if sum.Duration < time.Second {
		t.Errorf("Duration = %v, want it measured from start", sum.Duration)
	}
}

// TestTally_Empty verifies no results tally to an empty summary.
// Only the duration is set.
func TestTally_Empty(t *testing.T) {
	t.Parallel()
	sum := Tally(nil, time.Now())
	if sum.Planned != 0 || sum.Attempted != 0 || sum.Skipped != 0 {
		t.Errorf("summary = %+v, want nothing counted", sum)
	}
}

// TestRunner_Run_RefusedAdmissionSkips verifies an item refused an admission
// slot is skipped without running, and an admitted one releases its slot.
func TestRunner_Run_RefusedAdmissionSkips(t *testing.T) {
	t.Parallel()
	slots := &fakeAdmission{free: 1}
	runner := Runner[int]{Concurrency: 1, Admission: slots}
	var ran atomic.Int64
	sum := runner.Run(context.Background(), []int{1, 2}, func(context.Context, int) ItemResult {
		ran.Add(1)
		slots.free = 0
		return Succeeded()
	})
	if ran.Load() != 1 || sum.Succeeded != 1 || sum.Skipped != 1 {
		t.Errorf("ran=%d summary=%+v, want one run and one skipped", ran.Load(), sum)
	}
	if slots.released != 1 {
		t.Errorf("released = %d, want the admitted item's slot back", slots.released)
	}
}

// fakeAdmission hands out free slots and counts the ones given back.
type fakeAdmission struct {
	free     int
	released int
}

// AcquireAdmission takes a slot while one is free.
func (f *fakeAdmission) AcquireAdmission(context.Context) bool {
	if f.free == 0 {
		return false
	}
	f.free--
	return true
}

// ReleaseAdmission counts a slot given back.
func (f *fakeAdmission) ReleaseAdmission() { f.released++ }

// TestSummary_ReportedAndChanged pins what makes a cycle worth a completion
// entry and what makes it refresh the quota gauges.
func TestSummary_ReportedAndChanged(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name              string
		sum               Summary
		reported, changed bool
	}{
		{"idle", Summary{}, false, false},
		{"all skipped", Summary{Planned: 2, Skipped: 2}, true, false},
		{"only deferred", Summary{Deferred: 3}, true, false},
		{"one succeeded", Summary{Planned: 1, Succeeded: 1}, true, true},
	}
	for _, tc := range tests {
		if tc.sum.Reported() != tc.reported || tc.sum.Changed() != tc.changed {
			t.Errorf("%s: Reported, Changed = %v, %v; want %v, %v",
				tc.name, tc.sum.Reported(), tc.sum.Changed(), tc.reported, tc.changed)
		}
	}
}

// TestRunner_Run_ReportsItemUsage verifies the summary carries what the items
// were charged, and nothing charged outside the run.
func TestRunner_Run_ReportsItemUsage(t *testing.T) {
	t.Parallel()
	ctx, outer := audit.WithTally(context.Background())
	audit.TallyOf(ctx).Add("b1", audit.BackendUsage{APIRequests: 9})

	runner := Runner[int]{Concurrency: 2}
	sum := runner.Run(ctx, []int{1, 2}, func(ctx context.Context, _ int) ItemResult {
		audit.TallyOf(ctx).Add("b1", audit.BackendUsage{APIRequests: 1, EgressBytes: 10})
		return ItemResult{Outcome: ItemSucceeded}
	})

	if got, want := sum.Usage, (audit.Usage{"b1": {APIRequests: 2, EgressBytes: 20}}); !reflect.DeepEqual(got, want) {
		t.Errorf("Usage = %+v, want %+v", got, want)
	}
	if got := outer.Usage()["b1"].APIRequests; got != 11 {
		t.Errorf("enclosing tally APIRequests = %d, want 11", got)
	}
}
