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
	"sync"
)

// Run processes items concurrently with bounded parallelism. The fn callback
// is invoked once per item. If ctx is cancelled, remaining undispatched items
// are skipped. In-flight items run to completion.
func Run[T any](ctx context.Context, concurrency int, items []T, fn func(context.Context, T)) {
	if concurrency <= 0 {
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

// Collect processes items concurrently and returns one result per item,
// preserving input order. If ctx is cancelled, remaining undispatched items
// produce zero-value results.
func Collect[T any, R any](ctx context.Context, concurrency int, items []T, fn func(context.Context, T) R) []R {
	if concurrency <= 0 {
		concurrency = 1
	}
	results := make([]R, len(items))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

dispatch:
	for i, item := range items {
		select {
		case <-ctx.Done():
			break dispatch
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(idx int, it T) {
			defer func() { <-sem; wg.Done() }()
			results[idx] = fn(ctx, it)
		}(i, item)
	}

	wg.Wait()
	return results
}
