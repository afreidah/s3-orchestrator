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
	"net/http"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/cli/output"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
)

// stream issues a streaming request and renders the NDJSON event stream,
// returning the process exit code. Unlike do, it sets no client timeout: the
// server flushes a progress event per batch, so the connection stays active for
// the life of the operation and Ctrl-C cancels it.
func (c *client) stream(method, path, body string) int {
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}

	req, err := http.NewRequestWithContext(context.Background(), method, c.baseAddr+path, bodyReader)
	if err != nil {
		fmt.Fprintf(c.stderr, fmtError, err)
		return 1
	}
	req.Header.Set(adminTokenHeader, c.token)
	req.Header.Set("Accept", adminstream.ContentType)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := (&http.Client{}).Do(req) //nolint:gosec // G704: admin CLI target address is user-provided via --addr flag
	if err != nil {
		fmt.Fprintf(c.stderr, fmtError, err)
		return 1
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(resp.Body)
		c.renderError(data)
		return 1
	}

	return c.consumeStream(resp.Body)
}

// consumeStream decodes the response body one Event per line, rendering each
// and returning a non-zero exit code if any result reported failure.
func (c *client) consumeStream(body io.Reader) int {
	dec := json.NewDecoder(body)
	exit := 0
	for {
		var e adminstream.Event
		if err := dec.Decode(&e); err != nil {
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
	case adminstream.KindProgress:
		if e.Message != "" {
			fmt.Fprintf(c.stdout, "  %s\n", e.Message)
		} else {
			fmt.Fprintf(c.stdout, "  processed %d\n", e.Processed)
		}
	case adminstream.KindStepStart:
		c.renderStepStart(e)
	case adminstream.KindStepEnd:
		c.renderStepEnd(e)
	case adminstream.KindResult:
		return c.renderStreamResult(e)
	}
	return false
}

// stepLabelWidth is the column the step status ("OK"/"FAIL") is dotted out to,
// so per-object status markers line up regardless of key length.
const stepLabelWidth = 56

// renderStepStart writes the leading "<verb> <item> ......" with no newline and
// no trailing status, so the line appears the moment the item begins. The label
// (verb + item) is supplied whole by the server.
func (c *client) renderStepStart(e *adminstream.Event) {
	label := e.Message + " "
	dots := max(stepLabelWidth-len(label), 3)
	fmt.Fprintf(c.stdout, "  %s%s ", label, strings.Repeat(".", dots))
}

// renderStepEnd completes the current step line with the status marker and the
// per-object duration.
func (c *client) renderStepEnd(e *adminstream.Event) {
	status := strings.ToUpper(e.Outcome)
	if status == "" {
		status = "OK"
	}
	dur := output.FormatDuration(time.Duration(e.DurationMs) * time.Millisecond)
	if e.Message != "" {
		// Concurrent op: the whole line arrives at once with no preceding
		// step_start prefix, so render the label and dots here too.
		label := e.Message + " "
		dots := max(stepLabelWidth-len(label), 3)
		fmt.Fprintf(c.stdout, "  %s%s %-6s (%s)\n", label, strings.Repeat(".", dots), status, dur)
		return
	}
	fmt.Fprintf(c.stdout, "%-6s (%s)\n", status, dur)
}

// renderStreamResult prints the terminal result line in text mode, returning
// true on a failed outcome. A non-drained backfill hints to re-run.
func (c *client) renderStreamResult(e *adminstream.Event) bool {
	switch e.Outcome {
	case adminstream.OutcomeFailed:
		fmt.Fprintf(c.stderr, "error: %s\n", e.Error)
		return true
	case adminstream.OutcomeSkipped:
		fmt.Fprintf(c.stdout, "skipped: %s\n", e.Message)
	default:
		dur := output.FormatDuration(time.Duration(e.DurationMs) * time.Millisecond)
		summary := e.Message
		if summary == "" {
			summary = fmt.Sprintf("processed %d", e.Processed)
		}
		msg := fmt.Sprintf("done: %s (%s)", summary, dur)
		if drained, ok := e.Fields["done"].(bool); ok && !drained {
			msg += " - more remain, re-run to continue"
		}
		fmt.Fprintln(c.stdout, msg)
	}
	return false
}
