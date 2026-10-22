// -------------------------------------------------------------------------------
// Batch Runner Tests
//
// Author: Alex Freidah
//
// Covers tallying sequentially and concurrently, a cancelled context
// dispatching nothing while Planned still reflects the input, Tally over
// gathered results, and the cycle log line.
// -------------------------------------------------------------------------------

package batch

import (
	"bytes"
	"context"
	"log/slog"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
)

// TestRunner_Run_Tallies verifies sequential tallying of mixed outcomes.
func TestRunner_Run_Tallies(t *testing.T) {
	t.Parallel()
	items := []int{1, 2, 3, 4, 5}
	runner := Runner[int]{Name: "test", Concurrency: 1}
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
	runner := Runner[int]{Name: "test", Concurrency: 8}
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
	runner := Runner[int]{Name: "test", Concurrency: 4}
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

// TestLogCycle_WritesOneLine verifies LogCycle emits the named cycle line with
// the summary's counts and outcome.
func TestLogCycle_WritesOneLine(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	LogCycle(context.Background(), log, "sweep", Summary{Planned: 2, Attempted: 2, Succeeded: 1, Failed: 1})
	out := buf.String()
	for _, want := range []string{`msg="sweep cycle complete"`, "planned=2", "succeeded=1", "failed=1", "skipped=0", "outcome=" + OutcomePartial} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q missing %q", out, want)
		}
	}
	if strings.Count(out, "\n") != 1 {
		t.Errorf("log %q, want exactly one line", out)
	}
}

// TestLogCycle_NilLogger verifies a nil logger emits nothing and does not
// panic.
func TestLogCycle_NilLogger(t *testing.T) {
	t.Parallel()
	LogCycle(context.Background(), nil, "sweep", Summary{Planned: 1})
}

// TestRunner_Run_LogsCycle verifies a Runner with a logger reports its cycle
// through LogCycle.
func TestRunner_Run_LogsCycle(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	runner := Runner[int]{Name: "test", Log: slog.New(slog.NewTextHandler(&buf, nil)), Concurrency: 1}
	runner.Run(context.Background(), []int{1}, func(context.Context, int) ItemResult {
		return ItemResult{Outcome: ItemSucceeded}
	})
	if !strings.Contains(buf.String(), `msg="test cycle complete"`) {
		t.Errorf("log %q, want the cycle line", buf.String())
	}
}

// TestRunner_Run_ReportsItemUsage verifies the summary and the cycle line carry
// what the items were charged, and nothing charged outside the run.
func TestRunner_Run_ReportsItemUsage(t *testing.T) {
	t.Parallel()
	ctx, outer := audit.WithTally(context.Background())
	audit.TallyOf(ctx).Add("b1", audit.BackendUsage{APIRequests: 9})

	var buf bytes.Buffer
	runner := Runner[int]{Name: "test", Log: slog.New(slog.NewJSONHandler(&buf, nil)), Concurrency: 2}
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
	if !strings.Contains(buf.String(), `"usage":{"b1":{"api_requests":2,"egress_bytes":20,"ingress_bytes":0}}`) {
		t.Errorf("cycle line %q does not report the usage", buf.String())
	}
}
