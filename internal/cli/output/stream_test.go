// -------------------------------------------------------------------------------
// CLI Output - Stream Line Tests
//
// Author: Alex Freidah
//
// Pins the step and result wording the admin CLI and the TUI both print: the
// dotted status column, the OK default, and the re-run hint on a pass that
// stopped before draining its work.
// -------------------------------------------------------------------------------

package output

import (
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
)

// TestStepLabel verifies a short label is dotted out to the status column and
// a long one still gets a run of dots before its status.
func TestStepLabel(t *testing.T) {
	t.Parallel()
	if got := StepLabel("hashing k"); len(got) != stepLabelWidth+3 || !strings.HasPrefix(got, "  hashing k ...") {
		t.Errorf("StepLabel(short) = %q", got)
	}
	long := strings.Repeat("k", 80)
	if got := StepLabel(long); !strings.HasSuffix(got, long+" ... ") {
		t.Errorf("StepLabel(long) = %q, want three dots after the label", got)
	}
}

// TestStepStatus verifies the outcome is upper-cased with its duration, and
// a step that reported no outcome reads OK.
func TestStepStatus(t *testing.T) {
	t.Parallel()
	if got := StepStatus(&adminstream.Event{Outcome: "failed", DurationMs: 1200}); got != "FAILED (1.2s)" {
		t.Errorf("StepStatus(failed) = %q", got)
	}
	if got := StepStatus(&adminstream.Event{DurationMs: 40}); got != "OK     (40ms)" {
		t.Errorf("StepStatus(no outcome) = %q", got)
	}
}

// TestResult covers each outcome: a failure flagged with its error, a skip
// with its reason, and a done line with the count when no summary came and
// the re-run hint when the pass did not drain.
func TestResult(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		e      adminstream.Event
		want   string
		failed bool
	}{
		{"failed", adminstream.Event{Outcome: adminstream.OutcomeFailed, Error: "boom"}, "error: boom", true},
		{"skipped", adminstream.Event{Outcome: adminstream.OutcomeSkipped, Message: "factor 1"}, "skipped: factor 1", false},
		{"summary", adminstream.Event{Outcome: adminstream.OutcomeOK, Message: "moved 3", DurationMs: 2000}, "done: moved 3 (2.0s)", false},
		{"count", adminstream.Event{Processed: 7}, "done: processed 7 (0ms)", false},
		{"not drained", adminstream.Event{Processed: 1, Fields: map[string]any{"done": false}},
			"done: processed 1 (0ms) - more remain, re-run to continue", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			line, failed := Result(&c.e)
			if line != c.want || failed != c.failed {
				t.Errorf("Result = %q, %v; want %q, %v", line, failed, c.want, c.failed)
			}
		})
	}
}
