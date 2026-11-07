// -------------------------------------------------------------------------------
// Batch Runner - Per-Item Iteration, Tally and Reporting
//
// Author: Alex Freidah
//
// Runner takes a slice of work items through a per-item function with bounded
// concurrency, brackets each item in a progress step, tallies the outcomes into
// a Summary, and emits one "<name> cycle complete" log line, so every caller
// reports its work the same way.
// -------------------------------------------------------------------------------

package batch

import (
	"context"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/util/workerpool"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Runner drives one batch. Concurrency is passed through to workerpool.Run
// (1 = sequential). Admission, when set, runs each item under one of its
// slots and skips an item refused one. Key, when set, supplies each item's
// progress label, and Observer, when set, receives the per-item steps.
type Runner[T any] struct {
	Concurrency int
	Admission   Admitter
	Observer    progress.Observer
	Key         func(T) string
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Run processes items through fn with the configured concurrency, brackets each
// in a progress step, and returns the tallied Summary. Items not reached
// because ctx was cancelled mid-batch are left out of the tally, so
// Planned - (Attempted + Skipped) reflects the cancelled remainder. The items
// run under a usage tally, so the Summary reports what they were charged.
func (r Runner[T]) Run(ctx context.Context, items []T, fn func(context.Context, T) ItemResult) Summary {
	start := time.Now()
	ctx, usage := audit.WithTally(ctx)
	var (
		mu      sync.Mutex
		results = make([]ItemResult, 0, len(items))
	)
	workerpool.Run(ctx, r.Concurrency, items, func(ctx context.Context, item T) {
		var res ItemResult
		label := ""
		if r.Key != nil {
			label = r.Key(item)
		}
		progress.Track(r.Observer, label, func() string {
			res = r.runItem(ctx, item, fn)
			return res.Status
		})
		mu.Lock()
		results = append(results, res)
		mu.Unlock()
	})

	sum := Tally(results, start)
	sum.Planned = len(items)
	sum.Usage = usage.Usage()
	return sum
}

// runItem runs fn for one item, under an admission slot when the runner has
// admission.
func (r Runner[T]) runItem(ctx context.Context, item T, fn func(context.Context, T) ItemResult) ItemResult {
	if r.Admission == nil {
		return fn(ctx, item)
	}
	res := Skipped(progress.StatusSkipped)
	Admit(ctx, r.Admission, func() { res = fn(ctx, item) })
	return res
}

// Tally folds per-item results gathered without a Runner into a Summary,
// timed from start, for a caller that decides its items in phases of its own.
func Tally(results []ItemResult, start time.Time) Summary {
	sum := Summary{Planned: len(results), Duration: time.Since(start)}
	for _, res := range results {
		switch res.Outcome {
		case ItemSucceeded:
			sum.Succeeded++
		case ItemFailed:
			sum.Failed++
		case ItemSkipped:
			sum.Skipped++
		}
	}
	sum.Attempted = sum.Succeeded + sum.Failed
	return sum
}
