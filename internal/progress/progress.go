// -------------------------------------------------------------------------------
// Progress - Per-Item Observer for Long-Running Operations
//
// Author: Alex Freidah
//
// A shared, transport-agnostic observer that long-running operations (backfill,
// reconcile, scrub, replicate, purge) emit through to report per-item progress.
// Each unit of work brackets a start and an end event carrying the item label,
// outcome, and duration. A leaf package with no app dependencies so the worker
// and proxy layers can emit and the transport layer can render without a cycle.
// -------------------------------------------------------------------------------

package progress

import "time"

// Phase marks the start or end of one unit of work.
type Phase int

const (
	PhaseStart Phase = iota // the unit began
	PhaseEnd                // the unit finished
)

// Outcome values reported on a PhaseEnd step.
const (
	StatusOK     = "ok"
	StatusFailed = "failed"
)

// Step is one progress notification for a single unit of work. Status and
// Duration are set on PhaseEnd only.
type Step struct {
	Label    string // the item being processed (object key, backend name)
	Phase    Phase
	Status   string
	Duration time.Duration
}

// Observer receives a start and then an end step for each unit of work. A nil
// Observer means progress reporting is disabled.
type Observer func(Step)

// Track brackets one unit of work: it emits a start step, runs fn, then emits an
// end step with the status fn returns and the elapsed time. fn always runs, so a
// nil observer disables reporting without changing behavior.
func Track(o Observer, label string, fn func() string) {
	if o == nil {
		fn()
		return
	}
	o(Step{Label: label, Phase: PhaseStart})
	start := time.Now()
	status := fn()
	o(Step{Label: label, Phase: PhaseEnd, Status: status, Duration: max(time.Since(start), 0)})
}
