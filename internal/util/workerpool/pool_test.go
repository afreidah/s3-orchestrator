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

func TestRun_ProcessesAllItems(t *testing.T) {
	var count atomic.Int32
	items := []int{1, 2, 3, 4, 5}

	Run(context.Background(), 3, items, func(_ context.Context, _ int) {
		count.Add(1)
	})

	if got := count.Load(); got != 5 {
		t.Errorf("processed %d items, want 5", got)
	}
}

func TestRun_EmptySlice(t *testing.T) {
	var count atomic.Int32
	Run(context.Background(), 3, []int{}, func(_ context.Context, _ int) {
		count.Add(1)
	})
	if got := count.Load(); got != 0 {
		t.Errorf("processed %d items, want 0", got)
	}
}

func TestRun_ConcurrencyBound(t *testing.T) {
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

func TestRun_ZeroConcurrency(t *testing.T) {
	var count atomic.Int32
	items := []int{1, 2, 3}

	Run(context.Background(), 0, items, func(_ context.Context, _ int) {
		count.Add(1)
	})

	if got := count.Load(); got != 3 {
		t.Errorf("processed %d items, want 3", got)
	}
}

func TestRun_NegativeConcurrency(t *testing.T) {
	var count atomic.Int32
	items := []int{1, 2, 3}

	Run(context.Background(), -5, items, func(_ context.Context, _ int) {
		count.Add(1)
	})

	if got := count.Load(); got != 3 {
		t.Errorf("processed %d items, want 3", got)
	}
}

func TestRun_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var count atomic.Int32
	items := make([]int, 100)

	Run(ctx, 1, items, func(_ context.Context, _ int) {
		if count.Add(1) >= 3 {
			cancel()
		}
	})

	if got := count.Load(); got == int32(len(items)) {
		t.Errorf("processed all %d items despite cancellation", got)
	}
}

func TestRun_PassesContext(t *testing.T) {
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

func TestCollect_PreservesOrder(t *testing.T) {
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

func TestCollect_EmptySlice(t *testing.T) {
	results := Collect(context.Background(), 3, []int{}, func(_ context.Context, n int) int {
		return n
	})
	if len(results) != 0 {
		t.Errorf("len(results) = %d, want 0", len(results))
	}
}

func TestCollect_ConcurrencyBound(t *testing.T) {
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

func TestCollect_ContextCancellation(t *testing.T) {
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

func TestCollect_ZeroConcurrency(t *testing.T) {
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
