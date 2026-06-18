// -------------------------------------------------------------------------------
// Worker Batch Runner Tests
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"sync/atomic"
	"testing"
)

// TestWorkSummary_Outcome covers the four metric-label classifications.
func TestWorkSummary_Outcome(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name             string
		succeeded, faild int
		want             string
	}{
		{"all good", 3, 0, "success"},
		{"mixed", 2, 1, "partial"},
		{"all bad", 0, 2, "failed"},
		{"nothing", 0, 0, "empty"},
	}
	for _, c := range cases {
		s := WorkSummary{Succeeded: c.succeeded, Failed: c.faild}
		if got := s.Outcome(); got != c.want {
			t.Errorf("%s: Outcome() = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestBatchRunner_Run_Tallies verifies sequential tallying of mixed outcomes.
func TestBatchRunner_Run_Tallies(t *testing.T) {
	t.Parallel()
	items := []int{1, 2, 3, 4, 5}
	runner := BatchRunner[int]{Name: "test", Concurrency: 1}
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
	if sum.Outcome() != "partial" {
		t.Errorf("Outcome() = %q, want partial", sum.Outcome())
	}
}

// TestBatchRunner_Run_Concurrent verifies the tally is correct (and race-free)
// when items run with concurrency > 1.
func TestBatchRunner_Run_Concurrent(t *testing.T) {
	t.Parallel()
	const n = 200
	items := make([]int, n)
	for i := range items {
		items[i] = i
	}
	var ran atomic.Int64
	runner := BatchRunner[int]{Name: "test", Concurrency: 8}
	sum := runner.Run(context.Background(), items, func(_ context.Context, _ int) ItemResult {
		ran.Add(1)
		return ItemResult{Outcome: ItemSucceeded}
	})
	if int(ran.Load()) != n || sum.Succeeded != n || sum.Planned != n {
		t.Errorf("ran=%d summary=%+v, want all %d succeeded", ran.Load(), sum, n)
	}
}

// TestBatchRunner_Run_CancelledCtx verifies a pre-cancelled context dispatches
// nothing, leaving an empty summary.
func TestBatchRunner_Run_CancelledCtx(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner := BatchRunner[int]{Name: "test", Concurrency: 4}
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
