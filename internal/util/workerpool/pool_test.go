// -------------------------------------------------------------------------------
// Worker Pool Tests
//
// Author: Alex Freidah
//
// Unit tests for the generic bounded-concurrency worker pool. Validates
// concurrency limits, context cancellation, result ordering, and edge cases.
// -------------------------------------------------------------------------------

package workerpool

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// -------------------------------------------------------------------------
// RUN
// -------------------------------------------------------------------------

// TestRun_ProcessesAllItems verifies the run processes all items contract.
// Asserts that processed items, want 5.
func TestRun_ProcessesAllItems(t *testing.T) {
	t.Parallel()
	var count atomic.Int32
	items := []int{1, 2, 3, 4, 5}

	Run(context.Background(), 3, items, func(_ context.Context, _ int) {
		count.Add(1)
	})

	if got := count.Load(); got != 5 {
		t.Errorf("processed %d items, want 5", got)
	}
}

// TestRun_EmptySlice verifies the run empty slice contract.
// Asserts that processed items, want 0.
func TestRun_EmptySlice(t *testing.T) {
	t.Parallel()
	var count atomic.Int32
	Run(context.Background(), 3, []int{}, func(_ context.Context, _ int) {
		count.Add(1)
	})
	if got := count.Load(); got != 0 {
		t.Errorf("processed %d items, want 0", got)
	}
}

// TestRun_ConcurrencyBound verifies the run concurrency bound contract.
// Asserts that peak concurrency = , want <= 3.
func TestRun_ConcurrencyBound(t *testing.T) {
	t.Parallel()
	var active, peak atomic.Int32
	items := make([]int, 20)

	Run(context.Background(), 3, items, func(_ context.Context, _ int) {
		cur := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	})

	if p := peak.Load(); p > 3 {
		t.Errorf("peak concurrency = %d, want <= 3", p)
	}
	if p := peak.Load(); p < 2 {
		t.Errorf("peak concurrency = %d, expected at least 2 with 20 items", p)
	}
}

// TestRun_ZeroConcurrency verifies the run zero concurrency contract.
// Asserts that processed items, want 3.
func TestRun_ZeroConcurrency(t *testing.T) {
	t.Parallel()
	var count atomic.Int32
	items := []int{1, 2, 3}

	Run(context.Background(), 0, items, func(_ context.Context, _ int) {
		count.Add(1)
	})

	if got := count.Load(); got != 3 {
		t.Errorf("processed %d items, want 3", got)
	}
}

// TestRun_NegativeConcurrency verifies the run negative concurrency contract.
// Asserts that processed items, want 3.
func TestRun_NegativeConcurrency(t *testing.T) {
	t.Parallel()
	var count atomic.Int32
	items := []int{1, 2, 3}

	Run(context.Background(), -5, items, func(_ context.Context, _ int) {
		count.Add(1)
	})

	if got := count.Load(); got != 3 {
		t.Errorf("processed %d items, want 3", got)
	}
}

// TestRun_ContextCancellation verifies the run context cancellation contract.
// Asserts that processed all items despite cancellation.
func TestRun_ContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var count atomic.Int32
	items := make([]int, 100)

	Run(ctx, 1, items, func(_ context.Context, _ int) {
		if count.Add(1) >= 3 {
			cancel()
		}
	})

	if got := count.Load(); got == int32(len(items)) { //nolint:gosec // G115: test slice length, always small
		t.Errorf("processed all %d items despite cancellation", got)
	}
}

// TestRun_PassesContext verifies the run passes context contract.
// Asserts that context value = , want hello.
func TestRun_PassesContext(t *testing.T) {
	t.Parallel()
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "hello")

	Run(ctx, 1, []int{1}, func(ctx context.Context, _ int) {
		if v, ok := ctx.Value(ctxKey{}).(string); !ok || v != "hello" {
			t.Errorf("context value = %q, want hello", v)
		}
	})
}

// -------------------------------------------------------------------------
// COLLECT
// -------------------------------------------------------------------------

// TestCollect_PreservesOrder verifies the collect preserves order contract.
// Asserts that results[] = , want.
func TestCollect_PreservesOrder(t *testing.T) {
	t.Parallel()
	items := []int{10, 20, 30, 40, 50}

	results := Collect(context.Background(), 5, items, func(_ context.Context, n int) int {
		return n * 2
	})

	want := []int{20, 40, 60, 80, 100}
	for i, got := range results {
		if got != want[i] {
			t.Errorf("results[%d] = %d, want %d", i, got, want[i])
		}
	}
}

// TestCollect_EmptySlice verifies the collect empty slice contract.
// Asserts that len(results) = , want 0.
func TestCollect_EmptySlice(t *testing.T) {
	t.Parallel()
	results := Collect(context.Background(), 3, []int{}, func(_ context.Context, n int) int {
		return n
	})
	if len(results) != 0 {
		t.Errorf("len(results) = %d, want 0", len(results))
	}
}

// TestCollect_ConcurrencyBound verifies the collect concurrency bound contract.
// Asserts that peak concurrency = , want <= 4.
func TestCollect_ConcurrencyBound(t *testing.T) {
	t.Parallel()
	var active, peak atomic.Int32
	items := make([]int, 20)

	Collect(context.Background(), 4, items, func(_ context.Context, _ int) int {
		cur := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		return 0
	})

	if p := peak.Load(); p > 4 {
		t.Errorf("peak concurrency = %d, want <= 4", p)
	}
}

// TestCollect_ContextCancellation verifies the collect context cancellation contract.
// Asserts that len(results) = , want 100.
func TestCollect_ContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	items := make([]int, 100)
	for i := range items {
		items[i] = i + 1
	}

	results := Collect(ctx, 1, items, func(_ context.Context, n int) int {
		if n >= 3 {
			cancel()
		}
		return n * 10
	})

	if len(results) != 100 {
		t.Fatalf("len(results) = %d, want 100", len(results))
	}

	nonZero := 0
	for _, r := range results {
		if r != 0 {
			nonZero++
		}
	}
	if nonZero == 100 {
		t.Error("all items processed despite cancellation")
	}
}

// TestCollect_ZeroConcurrency verifies the collect zero concurrency contract.
// Asserts that results[] = , want.
func TestCollect_ZeroConcurrency(t *testing.T) {
	t.Parallel()
	results := Collect(context.Background(), 0, []int{1, 2, 3}, func(_ context.Context, n int) int {
		return n + 1
	})

	want := []int{2, 3, 4}
	for i, got := range results {
		if got != want[i] {
			t.Errorf("results[%d] = %d, want %d", i, got, want[i])
		}
	}
}
