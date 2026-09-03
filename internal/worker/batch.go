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

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// ItemOutcome classifies how a single work item finished, for the batch tally.
type ItemOutcome int

// ItemSkipped and the other per-item outcomes. Skipped is the zero value, so an
// item whose per-item function never ran - admission blocked it, say - counts
// as declined rather than as a success nobody performed.
const (
	ItemSkipped   ItemOutcome = iota // declined before any work
	ItemSucceeded                    // processed successfully
	ItemFailed                       // attempted but not completed
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
//
// Deferred is not an item outcome. It counts work the cycle never selected,
// because the backend holding it is over its usage limit, and those items were
// never in the batch at all. Reporting it alongside the per-item counts is what
// stops a budget-limited cycle reading as a complete one.
type WorkSummary struct {
	Planned   int           // items the cycle set out to process
	Attempted int           // items the per-item function ran (succeeded + failed)
	Succeeded int           // items that completed successfully
	Failed    int           // items attempted but not completed
	Skipped   int           // items declined before any work
	Deferred  int           // work never selected, the backend being over budget
	Duration  time.Duration // wall-clock time for the cycle
}

// The outcome label every worker reports its cycle under. Outcome() picks one
// of the first four from the item tally; OutcomeError is reported instead when
// a cycle failed before it had a tally to classify, such as the query that
// selects the batch.
const (
	OutcomeSuccess = "success"
	OutcomePartial = "partial"
	OutcomeFailed  = "failed"
	OutcomeEmpty   = "empty"
	OutcomeError   = "error"
)

// Outcome classifies the cycle for its runs-total metric: success (work done,
// no failures), partial (some succeeded, some failed), failed (only failures),
// or empty (nothing succeeded or failed). Reporting the tally rather than a
// hardcoded label is what lets an alert distinguish a cycle that did its work
// from one where every item failed.
func (s WorkSummary) Outcome() string {
	switch {
	case s.Failed == 0 && s.Succeeded > 0:
		return OutcomeSuccess
	case s.Succeeded > 0 && s.Failed > 0:
		return OutcomePartial
	case s.Failed > 0:
		return OutcomeFailed
	default:
		return OutcomeEmpty
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

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

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
