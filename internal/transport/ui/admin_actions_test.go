// -------------------------------------------------------------------------------
// UI Admin Actions Tests
//
// Author: Alex Freidah
//
// Covers the async dispatcher and status-polling helpers used by all four
// long-running admin buttons (Replicate, Scrub, Backfill Checksums,
// Encrypt Existing). The shared helpers handle method validation,
// not-configured guards, single-flight concurrency, and serialisation of
// asyncResult into the status JSON payload, so testing them once exercises
// the contract for every endpoint that delegates to them.
// -------------------------------------------------------------------------------

package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"log/slog"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/testutil/testx"
)

// fakeStatus stands in for a real action's response type: the shared state
// plus one named count, which is the shape every action publishes.
type fakeStatus struct {
	adminActionState
	Count  int `json:"count"`
	Failed int `json:"failed"`
	Total  int `json:"total"`
}

// fakeOp wraps run in an adminActionOp rendering fakeStatus, so the dispatcher
// tests exercise the generic paths without depending on a real operation.
func fakeOp(name string, run func(context.Context) (adminActionCounts, string, error)) adminActionOp[fakeStatus] {
	return adminActionOp[fakeStatus]{
		name: name,
		run:  run,
		render: func(s adminActionState, c adminActionCounts) fakeStatus {
			return fakeStatus{adminActionState: s, Count: c.Count, Failed: c.Failed, Total: c.Total}
		},
	}
}

// noopOp returns an adminActionOp whose run closure does nothing useful;
// used to exercise control-flow paths that exit before the goroutine fires.
func noopOp(name string) adminActionOp[fakeStatus] {
	return fakeOp(name, func(context.Context) (adminActionCounts, string, error) {
		return adminActionCounts{}, "", nil
	})
}

// TestStartAdminAction_MethodNotAllowed asserts that non-POST requests are
// rejected before the op is started.
func TestStartAdminAction_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	h := &Handler{log: slog.Default()}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/replicate", nil)
	w := httptest.NewRecorder()

	startAdminAction(h, w, req, noopOp("replicate"))

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

// TestStartAdminAction_AlreadyRunning asserts single-flight semantics: a
// second concurrent invocation returns 409 Conflict instead of clobbering
// the first run.
func TestStartAdminAction_AlreadyRunning(t *testing.T) {
	t.Parallel()
	h := &Handler{log: slog.Default()}
	if !h.asyncOps.TryStart("replicate") {
		t.Fatal("test pre-condition: TryStart should claim the slot")
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/replicate", nil)
	w := httptest.NewRecorder()
	startAdminAction(h, w, req, noopOp("replicate"))

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", w.Code, http.StatusConflict)
	}
}

// TestStartAdminAction_AcceptedStoresCounts asserts the happy path: 202 is
// returned immediately, the op runs in the background, and every count it
// reported ends up on the asyncResult for the poller.
func TestStartAdminAction_AcceptedStoresCounts(t *testing.T) {
	t.Parallel()
	h := &Handler{log: slog.Default()}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/encrypt-existing", nil)
	w := httptest.NewRecorder()

	startAdminAction(h, w, req, fakeOp("encrypt-existing-test",
		func(context.Context) (adminActionCounts, string, error) {
			return adminActionCounts{Count: 7, Failed: 1, Total: 8}, "", nil
		}))

	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want %d", w.Code, http.StatusAccepted)
	}

	res := waitForResult(t, h, "encrypt-existing-test")
	if !res.OK {
		t.Errorf("result.OK = false, want true")
	}
	want := adminActionCounts{Count: 7, Failed: 1, Total: 8}
	if res.Counts != want {
		t.Errorf("result.Counts = %+v, want %+v", res.Counts, want)
	}
}

// TestStartAdminAction_SkippedReason asserts that a skipped reason
// returned by the run closure is propagated onto the asyncResult so the
// status endpoint can render it.
func TestStartAdminAction_SkippedReason(t *testing.T) {
	t.Parallel()
	h := &Handler{log: slog.Default()}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/scrub", nil)
	w := httptest.NewRecorder()

	startAdminAction(h, w, req, fakeOp("scrub-test",
		func(context.Context) (adminActionCounts, string, error) {
			return adminActionCounts{}, "integrity verification is not enabled", nil
		}))

	res := waitForResult(t, h, "scrub-test")
	if res.Skipped != "integrity verification is not enabled" {
		t.Errorf("result.Skipped = %q", res.Skipped)
	}
}

// TestStartAdminAction_ErrorPropagates asserts that an error returned by
// the run closure is captured on the asyncResult.
func TestStartAdminAction_ErrorPropagates(t *testing.T) {
	t.Parallel()
	h := &Handler{log: slog.Default()}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/replicate", nil)
	w := httptest.NewRecorder()

	startAdminAction(h, w, req, fakeOp("replicate-err-test",
		func(context.Context) (adminActionCounts, string, error) {
			return adminActionCounts{}, "", errors.New("boom")
		}))

	res := waitForResult(t, h, "replicate-err-test")
	if res.Error == "" {
		t.Error("result.Error empty, expected non-empty")
	}
}

// -------------------------------------------------------------------------
// writeAdminActionStatus
// -------------------------------------------------------------------------

// TestWriteAdminActionStatus_Idle asserts that a never-started op surfaces
// as idle to the poller.
func TestWriteAdminActionStatus_Idle(t *testing.T) {
	t.Parallel()
	h := &Handler{log: slog.Default()}
	w := httptest.NewRecorder()
	writeAdminActionStatus(h, w, noopOp("never-started"))

	got := decodeBody(t, w)
	if got["status"] != "idle" {
		t.Errorf("status = %v, want idle (body=%v)", got["status"], got)
	}
}

// TestWriteAdminActionStatus_Running asserts that an in-flight op surfaces
// as running.
func TestWriteAdminActionStatus_Running(t *testing.T) {
	t.Parallel()
	h := &Handler{log: slog.Default()}
	if !h.asyncOps.TryStart("running-op") {
		t.Fatal("test pre-condition: TryStart")
	}
	w := httptest.NewRecorder()
	writeAdminActionStatus(h, w, noopOp("running-op"))

	got := decodeBody(t, w)
	if got["status"] != "running" {
		t.Errorf("status = %v, want running", got["status"])
	}
}

// TestWriteAdminActionStatus_Done_PropagatesCounts asserts that every count the
// operation reported appears in the status JSON, under the names the action's
// own response type gives them.
func TestWriteAdminActionStatus_Done_PropagatesCounts(t *testing.T) {
	t.Parallel()
	h := &Handler{log: slog.Default()}
	if !h.asyncOps.TryStart("done-op") {
		t.Fatal("test pre-condition: TryStart")
	}
	h.asyncOps.Complete("done-op", &asyncResult{
		OK:     true,
		Counts: adminActionCounts{Count: 5, Failed: 2, Total: 7},
	})

	w := httptest.NewRecorder()
	writeAdminActionStatus(h, w, noopOp("done-op"))

	got := decodeBody(t, w)
	if got["status"] != "done" {
		t.Errorf("status = %v, want done", got["status"])
	}
	if got["ok"] != true {
		t.Errorf("ok = %v, want true", got["ok"])
	}
	if got["count"] != float64(5) {
		t.Errorf("count = %v, want 5", got["count"])
	}
	if got["failed"] != float64(2) {
		t.Errorf("failed = %v, want 2", got["failed"])
	}
	if got["total"] != float64(7) {
		t.Errorf("total = %v, want 7", got["total"])
	}
}

// TestWriteAdminActionStatus_Skipped asserts that skipped results render
// as status=skipped with the supplied reason instead of done.
func TestWriteAdminActionStatus_Skipped(t *testing.T) {
	t.Parallel()
	h := &Handler{log: slog.Default()}
	if !h.asyncOps.TryStart("skip-op") {
		t.Fatal("test pre-condition: TryStart")
	}
	h.asyncOps.Complete("skip-op", &asyncResult{OK: true, Skipped: "factor <= 1"})

	w := httptest.NewRecorder()
	writeAdminActionStatus(h, w, noopOp("skip-op"))

	got := decodeBody(t, w)
	if got["status"] != "skipped" {
		t.Errorf("status = %v, want skipped", got["status"])
	}
	if got["reason"] != "factor <= 1" {
		t.Errorf("reason = %v, want %q", got["reason"], "factor <= 1")
	}
}

// TestWriteAdminActionStatus_Error asserts that errored results render as
// status=error with the supplied error message.
func TestWriteAdminActionStatus_Error(t *testing.T) {
	t.Parallel()
	h := &Handler{log: slog.Default()}
	if !h.asyncOps.TryStart("err-op") {
		t.Fatal("test pre-condition: TryStart")
	}
	h.asyncOps.Complete("err-op", &asyncResult{Error: "boom"})

	w := httptest.NewRecorder()
	writeAdminActionStatus(h, w, noopOp("err-op"))

	got := decodeBody(t, w)
	if got["status"] != "error" {
		t.Errorf("status = %v, want error", got["status"])
	}
	if got["error"] != "boom" {
		t.Errorf("error = %v, want boom", got["error"])
	}
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// waitForResult polls the asyncOpTracker until the named op's goroutine
// completes or the test deadline is hit. Encapsulates the goroutine sync
// so individual tests stay focused on assertions.
func waitForResult(t *testing.T, h *Handler, name string) *asyncResult {
	t.Helper()
	var result *asyncResult
	testx.Eventually(t, 2*time.Second, func() bool {
		res, running := h.asyncOps.Status(name)
		if running || res == nil {
			return false
		}
		result = res
		return true
	}, "op %q did not complete", name)
	return result
}

// decodeBody parses the recorded JSON response, failing the test on any
// decode error so callers can assert on a typed map.
func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v (body=%q)", err, w.Body.String())
	}
	return got
}

// -------------------------------------------------------------------------
// Per-wrapper smoke coverage. Each handleAPI* wrapper is a thin shim over
// startAdminAction (or writeAdminActionStatus) that closes over a fixed op
// name and one operation. Driving every wrapper's guard paths keeps the
// wrapper lines themselves covered; the runs themselves are exercised in
// admin_actions_integration_test.go.
// -------------------------------------------------------------------------

// TestAdminActionWrappers_IdleBeforeFirstRun asserts every status wrapper
// reports "idle" before its operation has ever been started, which is what
// the dashboard polls into on a fresh page load.
func TestAdminActionWrappers_IdleBeforeFirstRun(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		triggerPath string
		statusPath  string
		trigger     func(*Handler, http.ResponseWriter, *http.Request)
		status      func(*Handler, http.ResponseWriter, *http.Request)
	}{
		{"replicate", "/api/replicate", "/api/replicate/status",
			(*Handler).handleAPIReplicate, (*Handler).handleAPIReplicateStatus},
		{"scrub", "/api/scrub", "/api/scrub/status",
			(*Handler).handleAPIScrub, (*Handler).handleAPIScrubStatus},
		{"backfill-checksums", "/api/backfill-checksums", "/api/backfill-checksums/status",
			(*Handler).handleAPIBackfillChecksums, (*Handler).handleAPIBackfillChecksumsStatus},
		{"encrypt-existing", "/api/encrypt-existing", "/api/encrypt-existing/status",
			(*Handler).handleAPIEncryptExisting, (*Handler).handleAPIEncryptExistingStatus},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := &Handler{log: slog.Default()}

			statusReq := httptest.NewRequestWithContext(context.Background(), http.MethodGet, tc.statusPath, nil)
			statusW := httptest.NewRecorder()
			tc.status(h, statusW, statusReq)
			body := decodeBody(t, statusW)
			if body["status"] != "idle" {
				t.Errorf("status body = %v, want idle", body)
			}
		})
	}
}

// TestAdminActionWrappers_MethodNotAllowed asserts every trigger wrapper
// rejects non-POST requests before reaching the dispatcher's other
// guards.
func TestAdminActionWrappers_MethodNotAllowed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		path    string
		trigger func(*Handler, http.ResponseWriter, *http.Request)
	}{
		{"replicate", "/api/replicate", (*Handler).handleAPIReplicate},
		{"scrub", "/api/scrub", (*Handler).handleAPIScrub},
		{"backfill-checksums", "/api/backfill-checksums", (*Handler).handleAPIBackfillChecksums},
		{"encrypt-existing", "/api/encrypt-existing", (*Handler).handleAPIEncryptExisting},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := &Handler{log: slog.Default()}
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, tc.path, nil)
			w := httptest.NewRecorder()
			tc.trigger(h, w, req)
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
			}
		})
	}
}

// lifecycleHandler builds a UI handler whose expiry service is backed by stub.
func lifecycleHandler(t *testing.T, stub *uiExpiryStub) *Handler {
	t.Helper()
	var deps ops.LifecycleDeps
	if stub != nil {
		deps.Expiry = stub
	}
	return &Handler{log: slog.Default(), expiry: ops.NewLifecycle(deps)}
}

// uiExpiryStub stands in for *expiry.Manager with a fixed outcome.
type uiExpiryStub struct {
	cfg     *config.LifecycleConfig
	deleted int
	failed  int
}

// Config returns the configured rules, or nil when none are configured.
func (s *uiExpiryStub) Config() *config.LifecycleConfig { return s.cfg }

// ProcessRules reports the fixed outcome.
func (s *uiExpiryStub) ProcessRules(context.Context, []config.LifecycleRule) (int, int) {
	return s.deleted, s.failed
}

// TestHandleAPILifecycle_ReportsCountsThroughStatus drives the dashboard's
// path end to end: the trigger returns 202 and the poll reports what the sweep
// removed, under the key the button reads.
//
// Not parallel: the async tracker is per-handler but the op name is shared.
func TestHandleAPILifecycle_ReportsCountsThroughStatus(t *testing.T) {
	h := lifecycleHandler(t, &uiExpiryStub{
		cfg:     &config.LifecycleConfig{Rules: []config.LifecycleRule{{Prefix: "tmp/", ExpirationDays: 7}}},
		deleted: 9,
		failed:  1,
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/lifecycle", nil)
	w := httptest.NewRecorder()
	h.handleAPILifecycle(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("trigger status = %d, want 202", w.Code)
	}

	res := waitForResult(t, h, opLifecycle)
	if !res.OK || res.Skipped != "" {
		t.Fatalf("result = %+v, want a completed sweep", res)
	}

	statusW := httptest.NewRecorder()
	h.handleAPILifecycleStatus(statusW, httptest.NewRequestWithContext(
		context.Background(), http.MethodGet, "/api/lifecycle/status", nil))

	got := decodeBody(t, statusW)
	if got["status"] != "done" {
		t.Errorf("status = %v, want done", got["status"])
	}
	if got["deleted"] != float64(9) {
		t.Errorf("deleted = %v, want 9; the dashboard button keys on this", got["deleted"])
	}
	if got["failed"] != float64(1) {
		t.Errorf("failed = %v, want 1", got["failed"])
	}
}

// TestHandleAPILifecycle_NoRulesSurfacesTheReason holds the skip path: a
// deployment with no rules must say so rather than report a sweep of zero,
// which is the distinction the trigger exists to make.
//
// Not parallel: shares the op name with the test above.
func TestHandleAPILifecycle_NoRulesSurfacesTheReason(t *testing.T) {
	h := lifecycleHandler(t, &uiExpiryStub{})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/lifecycle", nil)
	h.handleAPILifecycle(httptest.NewRecorder(), req)

	res := waitForResult(t, h, opLifecycle)
	if res.Skipped == "" {
		t.Fatalf("result = %+v, want a skip naming the reason", res)
	}

	statusW := httptest.NewRecorder()
	h.handleAPILifecycleStatus(statusW, httptest.NewRequestWithContext(
		context.Background(), http.MethodGet, "/api/lifecycle/status", nil))

	got := decodeBody(t, statusW)
	if got["status"] != "skipped" || got["reason"] == "" {
		t.Errorf("status body = %v, want skipped with a reason", got)
	}
}
