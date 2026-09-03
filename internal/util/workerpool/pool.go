// -------------------------------------------------------------------------------
// Worker Pool - Generic Bounded Parallelism
//
// Author: Alex Freidah
//
// Provides generic bounded-concurrency worker pool functions for parallel
// processing of work items. Context cancellation stops dispatching new work;
// in-flight items run to completion.
// -------------------------------------------------------------------------------

package workerpool

import (
	"context"
	"log/slog"
	"sync"

	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
)

// Run processes items concurrently with bounded parallelism. The fn callback
// is invoked once per item. If ctx is cancelled, remaining undispatched items
// are skipped. In-flight items run to completion.
//
// Implementation note: spawns at most min(concurrency, len(items))
// worker goroutines that consume from a shared jobs channel until the
// dispatcher closes it. The earlier design spawned one goroutine per
// item bounded by a counting semaphore, which produced N goroutines for
// N items even though only `concurrency` ran at once - tens of thousands
// of transient goroutines on the larger cleanup/replication batches.
// The fixed-worker model keeps the same external semantics but caps
// goroutine churn at the worker count.
func Run[T any](ctx context.Context, concurrency int, items []T, fn func(context.Context, T)) {
	if concurrency <= 0 {
		slog.WarnContext(ctx, "concurrency <= 0, clamping to 1",
			logfmt.Component("workerpool"),
			"requested", concurrency,
		)
		concurrency = 1
	}

	n := len(items)
	if n == 0 {
		return
	}

	// A pre-cancelled context dispatches zero items deterministically.
	// Without this guard the multi-item dispatcher's select would race
	// between ctx.Done and a ready worker, occasionally leaking up to
	// `workers` items past cancellation.
	if ctx.Err() != nil {
		return
	}

	// Small-batch fast path. Single item skips goroutine + channel
	// setup entirely.
	if n == 1 {
		fn(ctx, items[0])
		return
	}

	workers := min(concurrency, n)

	jobs := make(chan T)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for item := range jobs {
				fn(ctx, item)
			}
		})
	}

	for _, item := range items {
		select {
		case <-ctx.Done():
			// Stop dispatching; close so idle workers exit and
			// in-flight workers finish their current item before
			// the range loop on jobs returns.
			close(jobs)
			wg.Wait()
			return
		case jobs <- item:
		}
	}
	close(jobs)
	wg.Wait()
}
