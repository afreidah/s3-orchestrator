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
)

// noopOp returns an adminActionOp whose run closure does nothing useful;
// used to exercise control-flow paths that exit before the goroutine fires.
func noopOp(name string) adminActionOp {
	return adminActionOp{
		name:      name,
		resultKey: "count",
		run: func(_ context.Context) (int, map[string]any, string, error) {
			return 0, nil, "", nil
		},
	}
}

// TestStartAdminAction_MethodNotAllowed asserts that non-POST requests are
// rejected before the op is started.
func TestStartAdminAction_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	h := &Handler{log: slog.Default()}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/replicate", nil)
	w := httptest.NewRecorder()

	h.startAdminAction(w, req, noopOp("replicate"))

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
	h.startAdminAction(w, req, noopOp("replicate"))

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", w.Code, http.StatusConflict)
	}
}

// TestStartAdminAction_AcceptedStoresExtra asserts the happy path: 202 is
// returned immediately, the op runs in the background, and counts plus
// op-specific Extra fields end up on the asyncResult for the poller.
func TestStartAdminAction_AcceptedStoresExtra(t *testing.T) {
	t.Parallel()
	h := &Handler{log: slog.Default()}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/encrypt-existing", nil)
	w := httptest.NewRecorder()

	h.startAdminAction(w, req, adminActionOp{
		name:      "encrypt-existing-test",
		resultKey: "encrypted",
		run: func(_ context.Context) (int, map[string]any, string, error) {
			return 7, map[string]any{"failed": 1, "total": 8}, "", nil
		},
	})

	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want %d", w.Code, http.StatusAccepted)
	}

	res := waitForResult(t, h, "encrypt-existing-test")
	if !res.OK {
		t.Errorf("result.OK = false, want true")
	}
	if res.Count != 7 {
		t.Errorf("result.Count = %d, want 7", res.Count)
	}
	if got, ok := res.Extra["failed"].(int); !ok || got != 1 {
		t.Errorf("result.Extra[failed] = %v, want int 1", res.Extra["failed"])
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

	h.startAdminAction(w, req, adminActionOp{
		name:      "scrub-test",
		resultKey: "checked",
		run: func(_ context.Context) (int, map[string]any, string, error) {
			return 0, nil, "integrity verification is not enabled", nil
		},
	})

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

	h.startAdminAction(w, req, adminActionOp{
		name:      "replicate-err-test",
		resultKey: "copies_created",
		run: func(_ context.Context) (int, map[string]any, string, error) {
			return 0, nil, "", errors.New("boom")
		},
	})

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
	h.writeAdminActionStatus(w, "never-started", "k")

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
	h.writeAdminActionStatus(w, "running-op", "k")

	got := decodeBody(t, w)
	if got["status"] != "running" {
		t.Errorf("status = %v, want running", got["status"])
	}
}

// TestWriteAdminActionStatus_Done_PropagatesExtra asserts that op-specific
// Extra fields (e.g. failed/total for encrypt-existing) appear in the
// status JSON payload alongside the resultKey.
func TestWriteAdminActionStatus_Done_PropagatesExtra(t *testing.T) {
	t.Parallel()
	h := &Handler{log: slog.Default()}
	if !h.asyncOps.TryStart("done-op") {
		t.Fatal("test pre-condition: TryStart")
	}
	h.asyncOps.Complete("done-op", &asyncResult{
		OK:    true,
		Count: 5,
		Extra: map[string]any{"failed": 2, "total": 7},
	})

	w := httptest.NewRecorder()
	h.writeAdminActionStatus(w, "done-op", "checked")

	got := decodeBody(t, w)
	if got["status"] != "done" {
		t.Errorf("status = %v, want done", got["status"])
	}
	if got["ok"] != true {
		t.Errorf("ok = %v, want true", got["ok"])
	}
	if got["checked"] != float64(5) {
		t.Errorf("checked = %v, want 5", got["checked"])
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
	h.writeAdminActionStatus(w, "skip-op", "k")

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
	h.writeAdminActionStatus(w, "err-op", "k")

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
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if result, running := h.asyncOps.Status(name); !running && result != nil {
			return result
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("op %q did not complete within deadline", name)
	return nil
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
