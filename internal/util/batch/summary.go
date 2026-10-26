// -------------------------------------------------------------------------------
// Batch Summary - Per-Item Outcomes and the Cycle Tally
//
// Author: Alex Freidah
//
// The vocabulary every batch reports in. A per-item function returns an
// ItemResult, the runner tallies those into a Summary, and Summary.Outcome
// turns the tally into the label the worker cycle metric carries, so an alert
// can tell a cycle that did its work from one where every item failed.
// -------------------------------------------------------------------------------

package batch

import (
	"log/slog"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/progress"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// ItemOutcome classifies how a single work item finished, for the tally.
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

// Succeeded is the result of an item that completed, reported as ok on the
// progress stream.
func Succeeded() ItemResult {
	return ItemResult{Outcome: ItemSucceeded, Status: progress.StatusOK}
}

// Failed is the result of an item attempted but not completed, reported as
// failed on the progress stream.
func Failed() ItemResult {
	return ItemResult{Outcome: ItemFailed, Status: progress.StatusFailed}
}

// Summary is the uniform result of one cycle of batch work.
//
// Deferred is not an item outcome: it counts work never selected because its
// backend is over its usage limit, so a budget-limited cycle does not read as
// complete. Usage is what the items were charged on each backend.
type Summary struct {
	Planned   int           // items the cycle set out to process
	Attempted int           // items the per-item function ran (succeeded + failed)
	Succeeded int           // items that completed successfully
	Failed    int           // items attempted but not completed
	Skipped   int           // items declined before any work
	Deferred  int           // work never selected, the backend being over budget
	Duration  time.Duration // wall-clock time for the cycle
	Usage     audit.Usage   // usage charged per backend while the items ran
}

// The outcome label every cycle reports under. Outcome() picks one of the first
// four from the item tally; OutcomeError is reported instead when a cycle failed
// before it had a tally to classify, such as the query that selects the batch.
const (
	OutcomeSuccess = "success"
	OutcomePartial = "partial"
	OutcomeFailed  = "failed"
	OutcomeEmpty   = "empty"
	OutcomeError   = "error"
)

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Plus adds two tallies, for a cycle that runs its work as several batches.
func (s Summary) Plus(o Summary) Summary {
	return Summary{
		Planned:   s.Planned + o.Planned,
		Attempted: s.Attempted + o.Attempted,
		Succeeded: s.Succeeded + o.Succeeded,
		Failed:    s.Failed + o.Failed,
		Skipped:   s.Skipped + o.Skipped,
		Deferred:  s.Deferred + o.Deferred,
		Duration:  s.Duration + o.Duration,
		Usage:     s.Usage.Plus(o.Usage),
	}
}

// AuditAttrs renders the tally as log attributes: the item counts and the usage
// charged to each backend.
func (s Summary) AuditAttrs() []slog.Attr {
	return []slog.Attr{
		slog.Int("planned", s.Planned),
		slog.Int("succeeded", s.Succeeded),
		slog.Int("failed", s.Failed),
		slog.Int("skipped", s.Skipped),
		slog.Int("deferred", s.Deferred),
		s.Usage.Attr(),
	}
}

// Outcome classifies the cycle for its cycle metric: success (work done,
// no failures), partial (some succeeded, some failed), failed (only failures),
// or empty (nothing succeeded or failed).
func (s Summary) Outcome() string {
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
