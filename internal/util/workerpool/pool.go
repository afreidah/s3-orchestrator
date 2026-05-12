// -------------------------------------------------------------------------------
// Worker Pool - Generic Bounded Parallelism
//
// Author: Alex Freidah
//
// Provides generic bounded-concurrency worker pool functions for parallel
// processing of work items. Context cancellation stops dispatching new work;
// in-flight items run to completion.
// -------------------------------------------------------------------------------

// Package workerpool provides generic bounded-concurrency worker
// pool functions for parallel processing of work items. Context
// cancellation stops dispatching new work; in-flight items run to
// completion.
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
func Run[T any](ctx context.Context, concurrency int, items []T, fn func(context.Context, T)) {
	if concurrency <= 0 {
		slog.WarnContext(ctx, "concurrency <= 0, clamping to 1",
			logfmt.Component("workerpool"),
			"requested", concurrency,
		)
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

dispatch:
	for _, item := range items {
		select {
		case <-ctx.Done():
			break dispatch
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(it T) {
			defer func() { <-sem; wg.Done() }()
			fn(ctx, it)
		}(item)
	}

	wg.Wait()
}
