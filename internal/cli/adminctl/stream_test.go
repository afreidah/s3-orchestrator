// -------------------------------------------------------------------------------
// Admin CLI - NDJSON Stream Consumer Tests
//
// Author: Alex Freidah
//
// Drives the streaming client against a fake server that emits NDJSON events,
// covering text-mode progress rendering, JSON passthrough, the failed-outcome
// exit code, the skipped outcome, and the HTTP-error path.
// -------------------------------------------------------------------------------

package adminctl

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/cli/output"
)

// ndjsonServer returns a server that writes the given raw NDJSON lines and
// records the Accept header it received.
func ndjsonServer(t *testing.T, lines string) (*httptest.Server, *string) {
	t.Helper()
	var accept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(lines))
	}))
	t.Cleanup(srv.Close)
	return srv, &accept
}

func TestStream_TextRendersStepsAndResult(t *testing.T) {
	t.Parallel()
	lines := `{"event":"start","op":"backfill-checksums"}
{"event":"step_start","message":"hashing photos/a.txt"}
{"event":"step_end","outcome":"ok","duration_ms":12}
{"event":"step_start","message":"hashing photos/b.txt"}
{"event":"step_end","outcome":"failed","duration_ms":3}
{"event":"result","outcome":"ok","processed":1,"duration_ms":1500,"fields":{"done":true}}
`
	srv, accept := ndjsonServer(t, lines)

	var stdout, stderr bytes.Buffer
	if code := Command("backfill-checksums", nil, srv.URL, "tok", &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d (stderr=%q)", code, stderr.String())
	}
	if !strings.Contains(*accept, "application/x-ndjson") {
		t.Errorf("Accept header = %q, want NDJSON", *accept)
	}
	out := stdout.String()

	// Each item renders on one line: "<verb> <item> .... OK (dur)".
	for _, want := range []string{
		"backfill-checksums started",
		"hashing photos/a.txt ", "OK     (12ms)",
		"hashing photos/b.txt ", "FAILED (3ms)",
		"done: processed 1 (1.5s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// The dotted prefix and its status share a line.
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, "photos/a.txt") && !strings.Contains(line, "OK") {
			t.Errorf("step start and end should be on one line: %q", line)
		}
	}
}

func TestStream_ConcurrentRendersLabeledLines(t *testing.T) {
	t.Parallel()
	// Concurrent ops (replicate, over-replication) emit a single labeled
	// step_end per finished item with no preceding step_start, so the client
	// renders the whole "<verb> <item> .... STATUS (dur)" line at once.
	lines := `{"event":"start","op":"replicate"}
{"event":"step_end","message":"replicating photos/a.txt","outcome":"ok","duration_ms":12}
{"event":"step_end","message":"replicating photos/b.txt","outcome":"failed","duration_ms":3}
{"event":"result","outcome":"ok","processed":1,"message":"created 1 copies","duration_ms":1500}
`
	srv, accept := ndjsonServer(t, lines)

	var stdout, stderr bytes.Buffer
	if code := Command("replicate", nil, srv.URL, "tok", &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d (stderr=%q)", code, stderr.String())
	}
	if !strings.Contains(*accept, "application/x-ndjson") {
		t.Errorf("Accept header = %q, want NDJSON", *accept)
	}
	out := stdout.String()
	for _, want := range []string{
		"replicate started",
		"replicating photos/a.txt ", "OK     (12ms)",
		"replicating photos/b.txt ", "FAILED (3ms)",
		"done: created 1 copies (1.5s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// The label, dots, and status all land on one line per item.
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, "photos/a.txt") && !strings.Contains(line, "OK") {
			t.Errorf("concurrent step should render on one line: %q", line)
		}
	}
}

func TestStream_NotDrainedHint(t *testing.T) {
	t.Parallel()
	lines := `{"event":"result","outcome":"ok","processed":500,"duration_ms":1000,"fields":{"done":false}}
`
	srv, _ := ndjsonServer(t, lines)
	var stdout bytes.Buffer
	Command("backfill-checksums", nil, srv.URL, "tok", &stdout, new(bytes.Buffer))
	if !strings.Contains(stdout.String(), "more remain, re-run to continue") {
		t.Errorf("output missing re-run hint:\n%s", stdout.String())
	}
}

func TestStream_JSONPassthrough(t *testing.T) {
	t.Parallel()
	lines := `{"event":"progress","processed":100}
{"event":"result","outcome":"ok","processed":100}
`
	srv, _ := ndjsonServer(t, lines)
	var stdout bytes.Buffer
	code := CommandWithFormat("backfill-checksums", nil, srv.URL, "tok", output.FormatJSON, &stdout, new(bytes.Buffer))
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	// JSON mode re-emits each event as its own NDJSON line.
	if strings.Count(strings.TrimSpace(stdout.String()), "\n") != 1 {
		t.Errorf("expected 2 JSON lines, got:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), `"event":"progress"`) {
		t.Errorf("JSON passthrough missing progress event:\n%s", stdout.String())
	}
}

func TestStream_FailedOutcomeExits1(t *testing.T) {
	t.Parallel()
	lines := `{"event":"start","op":"backfill-checksums"}
{"event":"result","outcome":"failed","error":"backend exploded"}
`
	srv, _ := ndjsonServer(t, lines)
	var stdout, stderr bytes.Buffer
	code := Command("backfill-checksums", nil, srv.URL, "tok", &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit = %d, want 1 on failed outcome", code)
	}
	if !strings.Contains(stderr.String(), "backend exploded") {
		t.Errorf("stderr missing error:\n%s", stderr.String())
	}
}

func TestStream_SkippedOutcome(t *testing.T) {
	t.Parallel()
	lines := `{"event":"result","outcome":"skipped","message":"integrity verification is not enabled"}
`
	srv, _ := ndjsonServer(t, lines)
	var stdout bytes.Buffer
	code := Command("backfill-checksums", nil, srv.URL, "tok", &stdout, new(bytes.Buffer))
	if code != 0 {
		t.Errorf("exit = %d, want 0 on skipped", code)
	}
	if !strings.Contains(stdout.String(), "skipped: integrity verification is not enabled") {
		t.Errorf("output missing skip line:\n%s", stdout.String())
	}
}

func TestStream_HTTPError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad token"}`))
	}))
	defer srv.Close()
	var stdout, stderr bytes.Buffer
	code := Command("backfill-checksums", nil, srv.URL, "tok", &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit = %d, want 1 on HTTP error", code)
	}
	if !strings.Contains(stderr.String(), "bad token") {
		t.Errorf("stderr missing error:\n%s", stderr.String())
	}
}
