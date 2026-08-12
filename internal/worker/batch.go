// -------------------------------------------------------------------------------
// Worker Batch Runner - Shared per-item iteration, tally, and reporting
//
// Author: Alex Freidah
//
// BatchRunner runs a slice of work items through a per-item function with
// bounded concurrency, brackets each item in a progress step, tallies the
// outcomes into a WorkSummary, and emits one consistent "<name> cycle complete"
// log line. It replaces the per-worker boilerplate of an atomic counter pair, a
// workerpool.Run call, and a hand-rolled completion log, so every worker reports
// its batch the same way (and feeds the same outcome label to metrics).
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/util/workerpool"
)

// ItemOutcome classifies how a single work item finished, for the batch tally.
type ItemOutcome int

const (
	// ItemSkipped marks an item the worker declined before doing any work. It
	// is the zero value, so an item whose per-item function never ran (e.g.
	// admission blocked it) counts as skipped rather than succeeded.
	ItemSkipped ItemOutcome = iota
	// ItemSucceeded marks an item the worker processed successfully.
	ItemSucceeded
	// ItemFailed marks an item the worker attempted but could not complete.
	ItemFailed
)

// ItemResult is what a per-item function returns: the outcome for the tally and
// a human-readable status for the progress stream (ignored when no observer is
// attached).
type ItemResult struct {
	Outcome ItemOutcome
	Status  string
}

// WorkSummary is the uniform result of one worker cycle. It replaces the ad-hoc
// count tuples (checked/failed, processed/failed, moved) each worker used to
// return, so partial-failure reporting and metric labels are consistent.
type WorkSummary struct {
	Planned   int // items the cycle set out to process
	Attempted int // items the per-item function ran (succeeded + failed)
	Succeeded int // items that completed successfully
	Failed    int // items attempted but not completed
	Skipped   int // items declined before any work
	// Deferred counts work this cycle never selected, because the backend
	// holding it is over its usage limit. It is not an item outcome: these
	// items were never in the batch, so reporting it alongside the per-item
	// counts is what stops a budget-limited cycle reading as a complete one.
	Deferred int
	Duration time.Duration // wall-clock time for the cycle
}

// Outcome classifies the cycle for metric labels: success (work done, no
// failures), partial (some succeeded, some failed), failed (only failures), or
// empty (nothing succeeded or failed).
func (s WorkSummary) Outcome() string {
	switch {
	case s.Failed == 0 && s.Succeeded > 0:
		return "success"
	case s.Succeeded > 0 && s.Failed > 0:
		return "partial"
	case s.Failed > 0:
		return "failed"
	default:
		return "empty"
	}
}

// BatchRunner drives one worker cycle. Concurrency is passed through to
// workerpool.Run (1 = sequential); Key, when set, supplies each item's progress
// label; Observer, when set, receives the per-item progress steps.
type BatchRunner[T any] struct {
	Name        string
	Log         *slog.Logger
	Concurrency int
	Observer    progress.Observer
	Key         func(T) string
}

// Run processes items through fn with the configured concurrency, brackets each
// in a progress step, and returns the tallied WorkSummary. Items not reached
// because ctx was cancelled mid-cycle are left out of the tally, so
// Planned - (Attempted + Skipped) reflects the cancelled remainder.
func (r BatchRunner[T]) Run(ctx context.Context, items []T, fn func(context.Context, T) ItemResult) WorkSummary {
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

	sum := WorkSummary{
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
