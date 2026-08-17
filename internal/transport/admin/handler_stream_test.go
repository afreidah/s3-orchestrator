// -------------------------------------------------------------------------------
// Admin Handler - Streaming Backfill Tests
//
// Author: Alex Freidah
//
// Drives handleBackfillChecksums with the NDJSON Accept header and asserts the
// streamed event sequence: a start event, a progress event per pass, and a
// terminal result carrying the outcome and the drained flag.
// -------------------------------------------------------------------------------

package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// decodeEvents parses an NDJSON response body into a slice of events.
func decodeEvents(t *testing.T, body []byte) []adminstream.Event {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	var events []adminstream.Event
	for {
		var e adminstream.Event
		if err := dec.Decode(&e); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode event: %v", err)
		}
		events = append(events, e)
	}
	return events
}

func streamReq(target string) *http.Request {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, target, nil)
	req.Header.Set("Accept", adminstream.ContentType)
	return req
}

func TestHandleBackfillChecksums_StreamsProgressAndResult(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	integrityWith(t, h,
		backendOpsStub{integrity: &config.IntegrityConfig{Enabled: true, ScrubberBatchSize: 50}},
		&scrubberStub{backfillProcessed: 10, backfillMore: true})

	w := httptest.NewRecorder()
	h.handleBackfillChecksums(w, streamReq("/admin/api/backfill-checksums?max=25"))

	if ct := w.Header().Get("Content-Type"); ct != adminstream.ContentType {
		t.Errorf("Content-Type = %q, want %q", ct, adminstream.ContentType)
	}
	events := decodeEvents(t, w.Body.Bytes())
	if len(events) < 3 {
		t.Fatalf("got %d events, want start + steps + result:\n%s", len(events), w.Body.String())
	}
	if events[0].Kind != adminstream.KindStart || events[0].Op != "backfill-checksums" {
		t.Errorf("first event = %+v, want start/backfill-checksums", events[0])
	}

	last := events[len(events)-1]
	if last.Kind != adminstream.KindResult || last.Outcome != adminstream.OutcomeOK {
		t.Errorf("last event = %+v, want result/ok", last)
	}
	// 10 processed per pass, stops once total >= 25: 3 passes => 30.
	if last.Processed != 30 {
		t.Errorf("result processed = %d, want 30", last.Processed)
	}
	if drained, _ := last.Fields["done"].(bool); drained {
		t.Errorf("result done = true, want false (max bound hit, backlog not drained)")
	}

	// Each object emits a start and an end event; the key is carried on start.
	stepStarts, stepEnds := countStepEvents(events)
	if stepStarts != 30 || stepEnds != 30 {
		t.Errorf("step events = %d start / %d end, want 30 / 30", stepStarts, stepEnds)
	}
	if !allStepStartsLabeled(events) {
		t.Error("step_start missing the object key")
	}
}

// countStepEvents tallies the step_start and step_end events in a stream.
func countStepEvents(events []adminstream.Event) (starts, ends int) {
	for _, e := range events {
		switch e.Kind {
		case adminstream.KindStepStart:
			starts++
		case adminstream.KindStepEnd:
			ends++
		}
	}
	return starts, ends
}

// allStepStartsLabeled reports whether every step_start carries its item key.
func allStepStartsLabeled(events []adminstream.Event) bool {
	for _, e := range events {
		if e.Kind == adminstream.KindStepStart && e.Message == "" {
			return false
		}
	}
	return true
}

func TestHandleBackfillChecksums_StreamsSkippedWhenDisabled(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	integrityWith(t, h, backendOpsStub{integrity: &config.IntegrityConfig{Enabled: false}}, &scrubberStub{})

	w := httptest.NewRecorder()
	h.handleBackfillChecksums(w, streamReq("/admin/api/backfill-checksums"))

	events := decodeEvents(t, w.Body.Bytes())
	last := events[len(events)-1]
	if last.Kind != adminstream.KindResult || last.Outcome != adminstream.OutcomeSkipped {
		t.Errorf("last event = %+v, want result/skipped", last)
	}
}

func TestHandleReconcile_StreamsProgressAndResult(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	h.reconciler = newReconciler(t, &worker.ReconcileResult{Imported: 4, Removed: 1, BackendsScanned: 2}, nil)

	w := httptest.NewRecorder()
	h.handleReconcile(w, streamReq("/admin/api/reconcile"))

	events := decodeEvents(t, w.Body.Bytes())
	if len(events) < 3 {
		t.Fatalf("got %d events, want start + progress + result:\n%s", len(events), w.Body.String())
	}
	if events[0].Kind != adminstream.KindStart || events[0].Op != "reconcile" {
		t.Errorf("first event = %+v, want start/reconcile", events[0])
	}
	last := events[len(events)-1]
	if last.Kind != adminstream.KindResult || last.Outcome != adminstream.OutcomeOK {
		t.Errorf("last event = %+v, want result/ok", last)
	}
	if imported, _ := last.Fields["imported"].(float64); imported != 4 {
		t.Errorf("result imported = %v, want 4", last.Fields["imported"])
	}
}

func TestHandleReconcile_StreamsFailure(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	h.reconciler = newReconciler(t, nil, errors.New("scan failed"))

	w := httptest.NewRecorder()
	h.handleReconcile(w, streamReq("/admin/api/reconcile"))

	events := decodeEvents(t, w.Body.Bytes())
	last := events[len(events)-1]
	if last.Kind != adminstream.KindResult || last.Outcome != adminstream.OutcomeFailed {
		t.Errorf("last event = %+v, want result/failed", last)
	}
	if last.Error != "scan failed" {
		t.Errorf("result error = %q, want 'scan failed'", last.Error)
	}
}

func TestHandleReplicate_StreamsProgressAndResult(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	replicationWith(t, h, replicatorStub{cfg: &config.ReplicationConfig{Factor: 2}, created: 3}, overRepStub{})

	w := httptest.NewRecorder()
	h.handleReplicate(w, streamReq("/admin/api/replicate"))

	if ct := w.Header().Get("Content-Type"); ct != adminstream.ContentType {
		t.Errorf("Content-Type = %q, want %q", ct, adminstream.ContentType)
	}
	events := decodeEvents(t, w.Body.Bytes())
	if events[0].Kind != adminstream.KindStart || events[0].Op != "replicate" {
		t.Errorf("first event = %+v, want start/replicate", events[0])
	}
	last := events[len(events)-1]
	if last.Kind != adminstream.KindResult || last.Outcome != adminstream.OutcomeOK {
		t.Errorf("last event = %+v, want result/ok", last)
	}
	if last.Processed != 3 {
		t.Errorf("result processed = %d, want 3", last.Processed)
	}
	if created, _ := last.Fields["copies_created"].(float64); created != 3 {
		t.Errorf("result copies_created = %v, want 3", last.Fields["copies_created"])
	}
	// Replication fans out across a worker pool, so steps render as complete
	// labeled lines (step_end only), never a live step_start prefix.
	stepStarts, steps := countStepEvents(events)
	if steps != 3 || stepStarts != 0 {
		t.Errorf("step events = %d step_end / %d step_start, want 3 / 0", steps, stepStarts)
	}
}

func TestHandleReplicate_StreamsSkippedWhenUnconfigured(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	replicationWith(t, h, replicatorStub{cfg: &config.ReplicationConfig{Factor: 1}}, overRepStub{})

	w := httptest.NewRecorder()
	h.handleReplicate(w, streamReq("/admin/api/replicate"))

	events := decodeEvents(t, w.Body.Bytes())
	last := events[len(events)-1]
	if last.Kind != adminstream.KindResult || last.Outcome != adminstream.OutcomeSkipped {
		t.Errorf("last event = %+v, want result/skipped", last)
	}
}

func TestHandleOverReplicationClean_StreamsProgressAndResult(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	replicationWith(t, h, replicatorStub{}, overRepStub{cfg: &config.ReplicationConfig{Factor: 2}, cleaned: 2})

	w := httptest.NewRecorder()
	h.handleOverReplicationClean(w, streamReq("/admin/api/over-replication"))

	events := decodeEvents(t, w.Body.Bytes())
	if events[0].Kind != adminstream.KindStart || events[0].Op != "over-replication" {
		t.Errorf("first event = %+v, want start/over-replication", events[0])
	}
	last := events[len(events)-1]
	if last.Kind != adminstream.KindResult || last.Outcome != adminstream.OutcomeOK {
		t.Errorf("last event = %+v, want result/ok", last)
	}
	if last.Processed != 2 {
		t.Errorf("result processed = %d, want 2", last.Processed)
	}
	stepStarts, steps := countStepEvents(events)
	if steps != 2 || stepStarts != 0 {
		t.Errorf("step events = %d step_end / %d step_start, want 2 / 0", steps, stepStarts)
	}
}

// TestReplicationStreams_CarryObjectsTheCycleCouldNotFinish asserts the
// terminal result of both streaming cycles reports the objects left behind.
// The stream is what the CLI and the TUI render, so a count missing here is a
// partial pass that reads as a complete one in every interactive client.
func TestReplicationStreams_CarryObjectsTheCycleCouldNotFinish(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		field string
		run   func(*Handler, *httptest.ResponseRecorder)
	}{
		{
			name:  "replicate",
			field: "copies_created",
			run: func(h *Handler, w *httptest.ResponseRecorder) {
				h.handleReplicate(w, streamReq("/admin/api/replicate"))
			},
		},
		{
			name:  "over-replication",
			field: "copies_removed",
			run: func(h *Handler, w *httptest.ResponseRecorder) {
				h.handleOverReplicationClean(w, streamReq("/admin/api/over-replication"))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newCoverageHandler(t)
			replicationWith(t, h,
				replicatorStub{cfg: &config.ReplicationConfig{Factor: 2}, created: 2, failed: 4},
				overRepStub{cfg: &config.ReplicationConfig{Factor: 2}, cleaned: 2, failed: 4})

			w := httptest.NewRecorder()
			tc.run(h, w)

			events := decodeEvents(t, w.Body.Bytes())
			last := events[len(events)-1]
			if last.Kind != adminstream.KindResult || last.Outcome != adminstream.OutcomeOK {
				t.Fatalf("last event = %+v, want result/ok", last)
			}
			if n, _ := last.Fields[tc.field].(float64); n != 2 {
				t.Errorf("result %s = %v, want 2", tc.field, last.Fields[tc.field])
			}
			if failed, _ := last.Fields["failed"].(float64); failed != 4 {
				t.Errorf("result failed = %v, want 4", last.Fields["failed"])
			}
		})
	}
}

// TestHandleRebalance_StreamsMoves asserts each move renders as its own line
// naming the object and the backends it travelled between, then a terminal
// result carrying the move count.
func TestHandleRebalance_StreamsMoves(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	rebalanceWith(t, h, &rebalancerStub{moved: 2})

	w := httptest.NewRecorder()
	h.handleRebalance(w, streamReq("/admin/api/rebalance"))

	events := decodeEvents(t, w.Body.Bytes())
	if events[0].Kind != adminstream.KindStart || events[0].Op != "rebalance" {
		t.Errorf("first event = %+v, want start/rebalance", events[0])
	}
	if !strings.Contains(events[1].Message, "moving obj-0  src -> dst") {
		t.Errorf("move line = %q, want the object and both backends", events[1].Message)
	}
	last := events[len(events)-1]
	if last.Kind != adminstream.KindResult || last.Outcome != adminstream.OutcomeOK || last.Processed != 2 {
		t.Errorf("last event = %+v, want result/ok with 2 processed", last)
	}
	// Moves run concurrently, so each is one completed line rather than a pair.
	stepStarts, steps := countStepEvents(events)
	if steps != 2 || stepStarts != 0 {
		t.Errorf("step events = %d step_end / %d step_start, want 2 / 0", steps, stepStarts)
	}
}

// TestHandleRebalance_StreamsSkip asserts a cycle that planned nothing ends the
// stream with the reason rather than an empty run of zero moves.
func TestHandleRebalance_StreamsSkip(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	rebalanceWith(t, h, &rebalancerStub{skip: worker.SkipReasonEmptyPlan})

	w := httptest.NewRecorder()
	h.handleRebalance(w, streamReq("/admin/api/rebalance"))

	events := decodeEvents(t, w.Body.Bytes())
	last := events[len(events)-1]
	if last.Outcome != adminstream.OutcomeSkipped || last.Message != worker.SkipReasonEmptyPlan {
		t.Errorf("last event = %+v, want a skip carrying the empty-plan reason", last)
	}
}

// TestHandleRebalance_StreamsFailure asserts a failed cycle terminates the
// stream with the error rather than a partial run the caller cannot classify.
func TestHandleRebalance_StreamsFailure(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	rebalanceWith(t, h, &rebalancerStub{err: errors.New("planning failed")})

	w := httptest.NewRecorder()
	h.handleRebalance(w, streamReq("/admin/api/rebalance"))

	events := decodeEvents(t, w.Body.Bytes())
	last := events[len(events)-1]
	if last.Kind != adminstream.KindResult || last.Outcome != adminstream.OutcomeFailed {
		t.Errorf("last event = %+v, want result/failed", last)
	}
	if last.Error != "planning failed" {
		t.Errorf("result error = %q, want 'planning failed'", last.Error)
	}
}

func TestHandleRemoveBackend_StreamsPurge(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	token := h.generateRemoveToken("b1")

	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete,
		"/admin/api/backends/b1?purge=true&confirm="+token, nil)
	req.SetPathValue("name", "b1")
	req.Header.Set("Accept", adminstream.ContentType)

	w := httptest.NewRecorder()
	h.handleRemoveBackend(w, req)

	if ct := w.Header().Get("Content-Type"); ct != adminstream.ContentType {
		t.Errorf("Content-Type = %q, want %q", ct, adminstream.ContentType)
	}
	events := decodeEvents(t, w.Body.Bytes())
	if events[0].Kind != adminstream.KindStart || events[0].Op != "remove-backend" {
		t.Errorf("first event = %+v, want start/remove-backend", events[0])
	}
	last := events[len(events)-1]
	if last.Kind != adminstream.KindResult || last.Outcome != adminstream.OutcomeOK {
		t.Errorf("last event = %+v, want result/ok", last)
	}
	if be, _ := last.Fields["backend"].(string); be != "b1" {
		t.Errorf("result backend = %v, want b1", last.Fields["backend"])
	}
}
