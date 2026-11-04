// -------------------------------------------------------------------------------
// Admin CLI - NDJSON Progress Stream Consumer
//
// Author: Alex Freidah
//
// Consumes the server's newline-delimited JSON progress stream for long-running
// commands. The request opts in via the Accept header and runs with no client
// timeout, since a stream outlives any fixed deadline and the server's progress
// events keep the connection active. Text mode renders live progress lines;
// JSON mode passes each event through as one NDJSON line.
// -------------------------------------------------------------------------------

package adminctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"
	"github.com/afreidah/s3-orchestrator/internal/cli/output"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
)

// stream issues a bodiless streaming request and renders the NDJSON event
// stream, returning the process exit code. Unlike do, it sets no client
// timeout: the server flushes an event per item, so the connection stays
// active for the life of the operation and Ctrl-C cancels it.
func (c *client) stream(method, path string) int {
	events, err := c.api.Stream(context.Background(), method, path, nil, nil)
	if err != nil {
		c.reportError(err)
		return 1
	}
	defer events.Close()

	return c.consumeStream(events)
}

// consumeStream decodes the response body one Event per line, rendering each
// and returning a non-zero exit code if any result reported failure.
func (c *client) consumeStream(events adminclient.EventStream) int {
	exit := 0
	for {
		e, err := events.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			fmt.Fprintf(c.stderr, "error reading stream: %v\n", err)
			return 1
		}
		if c.renderStreamEvent(&e) {
			exit = 1
		}
	}
	return exit
}

// renderStreamEvent renders one streamed event, returning true when it reports
// a failed outcome. JSON mode re-emits the event as an NDJSON line; text mode
// prints a human progress or status line.
func (c *client) renderStreamEvent(e *adminstream.Event) bool {
	if c.format.IsJSON() {
		_ = json.NewEncoder(c.stdout).Encode(e)
		return e.Outcome == adminstream.OutcomeFailed
	}
	switch e.Kind {
	case adminstream.KindStart:
		fmt.Fprintf(c.stdout, "%s started\n", e.Op)
	case adminstream.KindStepStart:
		// The label prints the moment the item begins, and its step_end
		// completes the line.
		fmt.Fprint(c.stdout, output.StepLabel(e.Message))
	case adminstream.KindStepEnd:
		c.renderStepEnd(e)
	case adminstream.KindResult:
		return c.renderStreamResult(e)
	}
	return false
}

// renderStepEnd completes the step line with its status. A concurrent pass
// sends no step_start, so its step_end carries the label and prints it too.
func (c *client) renderStepEnd(e *adminstream.Event) {
	if e.Message != "" {
		fmt.Fprint(c.stdout, output.StepLabel(e.Message))
	}
	fmt.Fprintln(c.stdout, output.StepStatus(e))
}

// renderStreamResult prints the terminal result line, a failure to stderr,
// returning true on a failed outcome.
func (c *client) renderStreamResult(e *adminstream.Event) bool {
	line, failed := output.Result(e)
	w := c.stdout
	if failed {
		w = c.stderr
	}
	fmt.Fprintln(w, line)
	return failed
}
