// -------------------------------------------------------------------------------
// UI Admin Actions Tests
//
// Author: Alex Freidah
//
// Covers the background runner and status poll every dashboard action shares:
// single-flight starts, refusal of malformed parameters, and how each way a
// pass can end reaches the poll. The passes themselves are admin.Pass and are
// tested with the admin API; admin_actions_integration_test.go drives them
// end to end through the dashboard.
// -------------------------------------------------------------------------------

package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/testutil/testx"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
)

// fakePass is a dashboard action that reports a fixed report.
type fakePass struct {
	rep admin.Report
}

// Name implements pass.
func (fakePass) Name() string { return "fake" }

// Run implements pass.
func (p fakePass) Run(context.Context, *slog.Logger) admin.Report { return p.rep }

// TestStartAction_AlreadyRunning asserts single-flight semantics: a second
// start while the first runs is refused with 409 rather than clobbering it.
func TestStartAction_AlreadyRunning(t *testing.T) {
	t.Parallel()
	h := &Handler{log: slog.Default()}
	if !h.asyncOps.TryStart("/api/fake") {
		t.Fatal("test pre-condition: TryStart should claim the slot")
	}

	w := httptest.NewRecorder()
	h.startAction(w, "/api/fake", fakePass{}, nil)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", w.Code, http.StatusConflict)
	}
}

// TestStartAction_InvalidRefusedBeforeStarting asserts a malformed request is
// a 400 and never starts the pass.
func TestStartAction_InvalidRefusedBeforeStarting(t *testing.T) {
	t.Parallel()
	h := &Handler{log: slog.Default()}

	w := httptest.NewRecorder()
	h.startAction(w, "/api/fake", fakePass{}, errors.New("max is not a count"))

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if _, running := h.asyncOps.Status("/api/fake"); running {
		t.Error("a malformed request started the pass")
	}
}

// TestStartAction_AcceptedKeepsTheReport asserts the happy path: 202 at once,
// and the pass's report kept for the poll once it finishes.
func TestStartAction_AcceptedKeepsTheReport(t *testing.T) {
	t.Parallel()
	h := &Handler{log: slog.Default()}

	w := httptest.NewRecorder()
	h.startAction(w, "/api/fake", fakePass{rep: admin.Report{Result: adminstream.OutcomeOK, Body: "done"}}, nil)

	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want %d", w.Code, http.StatusAccepted)
	}
	if res := waitForResult(t, h, "/api/fake"); res.Body != "done" {
		t.Errorf("report = %+v, want the pass's own", res)
	}
}

// TestWriteActionStatus_EachState asserts the poll before a run, during it,
// and after each way it can end.
func TestWriteActionStatus_EachState(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		running bool
		rep     *admin.Report
		want    actionStatus
	}{
		{name: "never started", want: actionStatus{Status: statusIdle}},
		{name: "running", running: true, want: actionStatus{Status: statusRunning}},
		{
			name: "ran",
			rep:  &admin.Report{Result: adminstream.OutcomeOK, Body: map[string]any{"moved": float64(3)}},
			want: actionStatus{Status: adminstream.OutcomeOK, Result: map[string]any{"moved": float64(3)}},
		},
		{
			name: "skipped",
			rep:  &admin.Report{Result: adminstream.OutcomeSkipped, Message: "factor <= 1", Body: map[string]any{}},
			want: actionStatus{Status: adminstream.OutcomeSkipped, Reason: "factor <= 1", Result: map[string]any{}},
		},
		{
			name: "failed",
			rep:  &admin.Report{Result: adminstream.OutcomeFailed, Message: "scrub failed"},
			want: actionStatus{Status: adminstream.OutcomeFailed, Error: "scrub failed"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := &Handler{log: slog.Default()}
			if tc.running || tc.rep != nil {
				h.asyncOps.TryStart("/api/fake")
			}
			if tc.rep != nil {
				h.asyncOps.Complete("/api/fake", tc.rep)
			}

			w := httptest.NewRecorder()
			h.writeActionStatus(w, "/api/fake")

			var got actionStatus
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(tc.want)
			if !bytes.Equal(gotJSON, wantJSON) {
				t.Errorf("status = %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

// TestRewriteAction_RefusesBadMax asserts a malformed max is reported as the
// request's error rather than read as absent, which would rewrite the fleet.
func TestRewriteAction_RefusesBadMax(t *testing.T) {
	t.Parallel()
	h := &Handler{log: slog.Default(), d: Deps{Ops: &ops.Services{}}}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/compress-existing?max=1O", nil)
	if _, err := rewriteAction(admin.CompressExisting)(h, r); err == nil {
		t.Error("a malformed max was accepted")
	}
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// waitForResult polls the tracker until the action under key finishes or the
// test deadline is hit.
func waitForResult(t *testing.T, h *Handler, key string) *admin.Report {
	t.Helper()
	var result *admin.Report
	testx.Eventually(t, 2*time.Second, func() bool {
		res, running := h.asyncOps.Status(key)
		if running || res == nil {
			return false
		}
		result = res
		return true
	}, "action %q did not complete", key)
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
// ROUTE TABLE
// -------------------------------------------------------------------------

// actionPaths is every async action's trigger path.
var actionPaths = []string{
	"/api/rebalance", "/api/clean-excess", "/api/lifecycle", "/api/replicate", "/api/scrub",
	"/api/backfill-checksums", "/api/encrypt-existing", "/api/compress-existing", "/api/decompress-existing",
}

// TestAdminActions_IdleBeforeFirstRun asserts every status poll reports idle
// before its action has ever started, which is what the dashboard polls into
// on a fresh page load.
func TestAdminActions_IdleBeforeFirstRun(t *testing.T) {
	t.Parallel()
	for _, path := range actionPaths {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			h := &Handler{log: slog.Default()}
			statusW := httptest.NewRecorder()
			serveRoute(h, statusW, httptest.NewRequestWithContext(context.Background(), http.MethodGet, path+"/status", nil))
			if body := decodeBody(t, statusW); body["status"] != statusIdle {
				t.Errorf("status body = %v, want idle", body)
			}
		})
	}
}

// TestAdminActions_MethodNotAllowed asserts every trigger refuses anything
// but POST before the action is built.
func TestAdminActions_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	for _, path := range actionPaths {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			w := httptest.NewRecorder()
			serveRoute(&Handler{log: slog.Default()}, w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil))
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
			}
		})
	}
}

// -------------------------------------------------------------------------
// LIFECYCLE END TO END
// -------------------------------------------------------------------------

// lifecycleHandler builds a UI handler whose expiry service is backed by stub,
// with the stub's lifecycle config as the running one.
func lifecycleHandler(t *testing.T, stub *uiExpiryStub) *Handler {
	t.Helper()
	running := &config.Config{}
	deps := ops.LifecycleDeps{Config: config.NewLive(running)}
	if stub != nil {
		deps.Expiry = stub
		if stub.cfg != nil {
			running.Lifecycle = *stub.cfg
		}
	}
	return &Handler{log: slog.Default(), d: Deps{Ops: &ops.Services{Lifecycle: ops.NewLifecycle(deps)}}}
}

// uiExpiryStub stands in for *expiry.Manager with a fixed outcome.
type uiExpiryStub struct {
	cfg     *config.LifecycleConfig
	deleted int
	failed  int
}

// ProcessRules reports the fixed outcome.
func (s *uiExpiryStub) ProcessRules(context.Context, []config.LifecycleRule, progress.Observer) batch.Summary {
	return batch.Summary{Succeeded: s.deleted, Failed: s.failed}
}

// TestHandleAPILifecycle_ReportsCountsThroughStatus drives the dashboard's
// path end to end: the trigger returns 202 and the poll reports what the sweep
// removed, in the admin API's own response under the key the button reads.
func TestHandleAPILifecycle_ReportsCountsThroughStatus(t *testing.T) {
	t.Parallel()
	h := lifecycleHandler(t, &uiExpiryStub{
		cfg:     &config.LifecycleConfig{Rules: []config.LifecycleRule{{Prefix: "tmp/", ExpirationDays: 7}}},
		deleted: 9,
		failed:  1,
	})

	w := httptest.NewRecorder()
	serveRoute(h, w, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/lifecycle", nil))
	if w.Code != http.StatusAccepted {
		t.Fatalf("trigger status = %d, want 202", w.Code)
	}
	waitForResult(t, h, "/api/lifecycle")

	statusW := httptest.NewRecorder()
	serveRoute(h, statusW, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/lifecycle/status", nil))

	got := decodeBody(t, statusW)
	result, _ := got["result"].(map[string]any)
	if got["status"] != adminstream.OutcomeOK || result["deleted"] != float64(9) || result["failed"] != float64(1) {
		t.Errorf("status body = %v, want ok with deleted 9 and failed 1", got)
	}
}

// TestHandleAPILifecycle_NoRulesSurfacesTheReason holds the skip path: a
// deployment with no rules must say so rather than report a sweep of zero,
// which is the distinction the trigger exists to make.
func TestHandleAPILifecycle_NoRulesSurfacesTheReason(t *testing.T) {
	t.Parallel()
	h := lifecycleHandler(t, &uiExpiryStub{})

	serveRoute(h, httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/lifecycle", nil))
	waitForResult(t, h, "/api/lifecycle")

	statusW := httptest.NewRecorder()
	serveRoute(h, statusW, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/lifecycle/status", nil))

	if got := decodeBody(t, statusW); got["status"] != adminstream.OutcomeSkipped || got["reason"] == "" {
		t.Errorf("status body = %v, want skipped with a reason", got)
	}
}
