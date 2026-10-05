// -------------------------------------------------------------------------------
// Admin Run-Worker Handler Tests
//
// Author: Alex Freidah
//
// Covers POST /admin/api/workers/{name}/run: the stream carries every line
// the run logs and then its outcome, a worker that did not run is reported
// as skipped with the reason, an unknown name is a 404 before anything
// starts, and the one-shot JSON form reports the same outcomes.
// -------------------------------------------------------------------------------

package admin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
)

// workerLogger logs through a tee handler, as the real process does, so a
// record logged with a tapped context reaches the tap. The primary handler
// writes to a buffer nothing reads; it has to be one that handles every
// level, since the tee only sees what its primary enables.
var workerLogger = slog.New(telemetry.NewTeeHandler(slog.NewTextHandler(&bytes.Buffer{}, nil), telemetry.NewLogBuffer()))

// runWorkerHandler returns a handler that knows the replication worker and
// runs it with run.
func runWorkerHandler(t *testing.T, run func(ctx context.Context, name string) error) http.Handler {
	t.Helper()
	h := newTestHandler(t)
	h.workerHealth = func() []adminapi.WorkerHealth { return []adminapi.WorkerHealth{{Name: "replication"}} }
	h.runWorker = run
	mux := http.NewServeMux()
	h.Register(mux)
	return mux
}

// postRun sends a run request for name, as a stream or as one-shot JSON.
func postRun(t *testing.T, mux http.Handler, name string, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/admin/api/workers/"+name+"/run", nil)
	if stream {
		req.Header.Set("Accept", adminstream.ContentType)
	}
	signRoot(t, req)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// events decodes an NDJSON body.
func events(t *testing.T, body string) []adminstream.Event {
	t.Helper()
	var out []adminstream.Event
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		var e adminstream.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("decode %q: %v", sc.Text(), err)
		}
		out = append(out, e)
	}
	return out
}

// TestRunWorker_StreamsWhatTheRunLogs verifies the stream opens, carries each
// record the run logs (with the level ahead of a warning and without the
// component), and closes with the outcome.
func TestRunWorker_StreamsWhatTheRunLogs(t *testing.T) {
	t.Parallel()
	mux := runWorkerHandler(t, func(ctx context.Context, name string) error {
		log := workerLogger.With(logfmt.Component(name))
		log.InfoContext(ctx, "copied object", "key", "photos/a.jpg")
		log.WarnContext(ctx, "source read failed", "backend", "c2")
		return nil
	})
	w := postRun(t, mux, "replication", true)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}

	evs := events(t, w.Body.String())
	if len(evs) != 4 {
		t.Fatalf("events = %+v, want start, two lines, result", evs)
	}
	if evs[0].Kind != adminstream.KindStart || evs[0].Op != "run replication" {
		t.Errorf("first event = %+v, want the start of the run", evs[0])
	}
	if evs[1].Message != "copied object key=photos/a.jpg" {
		t.Errorf("first line = %q", evs[1].Message)
	}
	if evs[2].Message != "WARN source read failed backend=c2" {
		t.Errorf("warning line = %q", evs[2].Message)
	}
	if evs[3].Kind != adminstream.KindResult || evs[3].Outcome != adminstream.OutcomeOK {
		t.Errorf("last event = %+v, want an ok result", evs[3])
	}
}

// TestRunWorker_StreamOutcomes verifies a run that did not happen is skipped
// with the reason, and one that failed carries its error.
func TestRunWorker_StreamOutcomes(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		err     error
		outcome string
		text    string
	}{
		"busy":     {err: lifecycle.ErrWorkerBusy, outcome: adminstream.OutcomeSkipped, text: reasonWorkerBusy},
		"disabled": {err: lifecycle.ErrWorkerDisabled, outcome: adminstream.OutcomeSkipped, text: reasonWorkerDisabled},
		"failed":   {err: errors.New("pass failed: boom"), outcome: adminstream.OutcomeFailed, text: "boom"},
	}
	for name, tc := range cases {
		mux := runWorkerHandler(t, func(context.Context, string) error { return tc.err })
		evs := events(t, postRun(t, mux, "replication", true).Body.String())
		last := evs[len(evs)-1]
		if last.Outcome != tc.outcome || !strings.Contains(last.Message+last.Error, tc.text) {
			t.Errorf("%s: result = %+v, want %s with %q", name, last, tc.outcome, tc.text)
		}
	}
}

// TestRunWorker_OneShot verifies the JSON form reports ok and skipped in the
// body and a failure as an error status.
func TestRunWorker_OneShot(t *testing.T) {
	t.Parallel()
	ok := postRun(t, runWorkerHandler(t, func(context.Context, string) error { return nil }), "replication", false)
	var resp adminapi.WorkerRunResponse
	if err := json.Unmarshal(ok.Body.Bytes(), &resp); err != nil || resp.Status != statusOK || resp.Worker != "replication" {
		t.Errorf("ok run = %d %s", ok.Code, ok.Body.String())
	}

	busy := postRun(t, runWorkerHandler(t, func(context.Context, string) error { return lifecycle.ErrWorkerBusy }), "replication", false)
	if err := json.Unmarshal(busy.Body.Bytes(), &resp); err != nil || resp.Status != statusSkipped || resp.Reason != reasonWorkerBusy {
		t.Errorf("busy run = %d %s", busy.Code, busy.Body.String())
	}

	failed := postRun(t, runWorkerHandler(t, func(context.Context, string) error { return errors.New("boom") }), "replication", false)
	if failed.Code != http.StatusInternalServerError {
		t.Errorf("failed run status = %d, want 500", failed.Code)
	}
}

// TestRunWorker_UnknownAndUnwired verifies a name the worker list does not
// show is a 404 that runs nothing, and a deployment with no worker pool
// answers 503.
func TestRunWorker_UnknownAndUnwired(t *testing.T) {
	t.Parallel()
	ran := false
	mux := runWorkerHandler(t, func(context.Context, string) error { ran = true; return nil })
	if w := postRun(t, mux, "nonesuch", true); w.Code != http.StatusNotFound || ran {
		t.Errorf("unknown worker: status=%d ran=%v, want 404 and nothing run", w.Code, ran)
	}

	h := newTestHandler(t)
	unwired := http.NewServeMux()
	h.Register(unwired)
	if w := postRun(t, unwired, "replication", true); w.Code != http.StatusServiceUnavailable {
		t.Errorf("unwired: status = %d, want 503", w.Code)
	}
}
