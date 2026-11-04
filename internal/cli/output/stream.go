// -------------------------------------------------------------------------------
// CLI Output - Stream Lines
//
// Author: Alex Freidah
//
// The words a streamed admin operation renders in: each step as its label
// dotted out to a status column, and the terminal result as a done, skipped or
// error line. The admin CLI writes them as they arrive and the TUI appends
// them to its output pane, so an operation reads the same on both.
// -------------------------------------------------------------------------------

package output

import (
	"fmt"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
	"github.com/afreidah/s3-orchestrator/internal/util/humanize"
)

// stepLabelWidth is the column a step's status is dotted out to, so the
// status words line up whatever the length of the item.
const stepLabelWidth = 56

// StepLabel renders a step's label indented and dotted out to the status
// column, ready for StepStatus to complete the line.
func StepLabel(label string) string {
	label += " "
	return "  " + label + strings.Repeat(".", max(stepLabelWidth-len(label), 3)) + " "
}

// StepStatus renders how a step ended and how long it took, upper-cased and
// padded so the durations line up, with OK for a step that reported none.
func StepStatus(e *adminstream.Event) string {
	status := strings.ToUpper(e.Outcome)
	if status == "" {
		status = "OK"
	}
	return fmt.Sprintf("%-6s (%s)", status, elapsed(e))
}

// Result renders the terminal result line and reports whether it is a
// failure. A pass that stopped before draining its work says to run it again.
func Result(e *adminstream.Event) (line string, failed bool) {
	switch e.Outcome {
	case adminstream.OutcomeFailed:
		return "error: " + e.Error, true
	case adminstream.OutcomeSkipped:
		return "skipped: " + e.Message, false
	}
	summary := e.Message
	if summary == "" {
		summary = fmt.Sprintf("processed %d", e.Processed)
	}
	line = fmt.Sprintf("done: %s (%s)", summary, elapsed(e))
	if drained, ok := e.Fields["done"].(bool); ok && !drained {
		line += " - more remain, re-run to continue"
	}
	return line, false
}

// elapsed renders the event's duration.
func elapsed(e *adminstream.Event) string {
	return humanize.Elapsed(time.Duration(e.DurationMs) * time.Millisecond)
}
