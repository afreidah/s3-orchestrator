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
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/util/workerpool"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Runner drives one batch. Concurrency is passed through to workerpool.Run
// (1 = sequential); Key, when set, supplies each item's progress label;
// Observer, when set, receives the per-item progress steps.
type Runner[T any] struct {
	Name        string
	Log         *slog.Logger
	Concurrency int
	Observer    progress.Observer
	Key         func(T) string
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Run processes items through fn with the configured concurrency, brackets each
// in a progress step, and returns the tallied Summary. Items not reached
// because ctx was cancelled mid-batch are left out of the tally, so
// Planned - (Attempted + Skipped) reflects the cancelled remainder.
func (r Runner[T]) Run(ctx context.Context, items []T, fn func(context.Context, T) ItemResult) Summary {
	start := time.Now()
	var succeeded, failed, skipped atomic.Int64

	workerpool.Run(ctx, r.Concurrency, items, func(ctx context.Context, item T) {
		var res ItemResult
		label := ""
		if r.Key != nil {
			label = r.Key(item)
		}
		progress.Track(r.Observer, label, func() string {
			res = fn(ctx, item)
			return res.Status
		})
		switch res.Outcome {
		case ItemSucceeded:
			succeeded.Add(1)
		case ItemFailed:
			failed.Add(1)
		case ItemSkipped:
			skipped.Add(1)
		}
	})

	sum := Summary{
		Planned:   len(items),
		Succeeded: int(succeeded.Load()),
		Failed:    int(failed.Load()),
		Skipped:   int(skipped.Load()),
		Duration:  time.Since(start),
	}
	sum.Attempted = sum.Succeeded + sum.Failed
	if r.Log != nil {
		r.Log.InfoContext(ctx, r.Name+" cycle complete",
			"planned", sum.Planned,
			"succeeded", sum.Succeeded,
			"failed", sum.Failed,
			"skipped", sum.Skipped,
			"outcome", sum.Outcome(),
			"duration", sum.Duration,
		)
	}
	return sum
}
