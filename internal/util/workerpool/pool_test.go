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
	"testing/synctest"
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
	synctest.Test(t, func(t *testing.T) {
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
	})
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

// TestRun_SingleItemFastPath ensures the inline fast path runs fn
// exactly once and propagates the input context. The fast path skips
// goroutine + channel setup entirely, so this guards against a
// future regression that bypasses it for n==1.
func TestRun_SingleItemFastPath(t *testing.T) {
	t.Parallel()
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "fastpath")
	var count atomic.Int32

	Run(ctx, 8, []int{42}, func(ctx context.Context, item int) {
		count.Add(1)
		if item != 42 {
			t.Errorf("item = %d, want 42", item)
		}
		if v, _ := ctx.Value(ctxKey{}).(string); v != "fastpath" {
			t.Errorf("context value = %q, want fastpath", v)
		}
	})

	if got := count.Load(); got != 1 {
		t.Errorf("processed %d items, want 1", got)
	}
}

// TestRun_PreCancelledContext locks in that a context cancelled
// before Run is called dispatches zero items. The pre-#861
// implementation could leak one item into a worker before the
// cancellation was observed; the fixed-worker dispatcher checks
// ctx.Done in the same select that hands items off, so the
// guarantee is now tight.
func TestRun_PreCancelledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var count atomic.Int32
	items := make([]int, 32)

	Run(ctx, 4, items, func(_ context.Context, _ int) {
		count.Add(1)
	})

	if got := count.Load(); got != 0 {
		t.Errorf("processed %d items under pre-cancelled ctx, want 0", got)
	}
}
