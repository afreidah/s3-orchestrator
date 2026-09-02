// -------------------------------------------------------------------------------
// Admin API - Lifecycle Handler Tests
//
// Author: Alex Freidah
//
// The on-demand expiration sweep. What the endpoint owes an operator who has
// just written a rule is the ability to tell three states apart: the rule
// matched and deleted, the rule matched nothing, and there is no rule at all.
// -------------------------------------------------------------------------------

package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
)

// expiryStub stands in for *expiry.Manager, reporting a fixed outcome for
// whatever rules it is handed.
type expiryStub struct {
	cfg     *config.LifecycleConfig
	deleted int
	failed  int
	called  bool
}

// Config returns the configured rules, or nil for a deployment with none.
func (s *expiryStub) Config() *config.LifecycleConfig { return s.cfg }

// ProcessRules records that the sweep ran, brackets one step per object it
// claims to have handled, and reports the fixed outcome.
func (s *expiryStub) ProcessRules(_ context.Context, _ []config.LifecycleRule, obs progress.Observer) (int, int) {
	s.called = true
	for i := range s.deleted {
		progress.Track(obs, fmt.Sprintf("gone-%d", i), func() string { return progress.StatusOK })
	}
	for i := range s.failed {
		progress.Track(obs, fmt.Sprintf("stuck-%d", i), func() string { return progress.StatusFailed })
	}
	return s.deleted, s.failed
}

// lifecycleWith installs a lifecycle service over the stub. A nil stub stands
// for a deployment whose expiry manager was never wired.
func lifecycleWith(t *testing.T, h *Handler, stub *expiryStub) {
	t.Helper()
	var deps ops.LifecycleDeps
	if stub != nil {
		deps.Expiry = stub
	}
	h.expiry = ops.NewLifecycle(deps)
}

// postLifecycle drives the endpoint and decodes its response.
func postLifecycle(t *testing.T, h *Handler) (*httptest.ResponseRecorder, adminapi.LifecycleResponse) {
	t.Helper()
	w := httptest.NewRecorder()
	h.handleLifecycle(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/admin/api/lifecycle", nil))

	var got adminapi.LifecycleResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v (body=%q)", err, w.Body.String())
	}
	return w, got
}

// oneRule is a minimal configured ruleset.
func oneRule() *config.LifecycleConfig {
	return &config.LifecycleConfig{Rules: []config.LifecycleRule{{Prefix: "tmp/", ExpirationDays: 7}}}
}

// TestHandleLifecycle_ReportsWhatItDeleted covers the answer an operator is
// after: the rule matched, and this is how much it removed.
func TestHandleLifecycle_ReportsWhatItDeleted(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	stub := &expiryStub{cfg: oneRule(), deleted: 12, failed: 1}
	lifecycleWith(t, h, stub)

	w, got := postLifecycle(t, h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !stub.called {
		t.Error("the sweep never ran")
	}
	if got.Status != statusOK || got.Deleted != 12 || got.Failed != 1 {
		t.Errorf("response = %+v, want ok/12/1", got)
	}
}

// TestHandleLifecycle_MatchedNothingIsNotASkip is the distinction the endpoint
// exists for: a rule that ran and matched nothing reports ok with zero, not a
// skip. A skip would read as "nothing happened" when something did.
func TestHandleLifecycle_MatchedNothingIsNotASkip(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	lifecycleWith(t, h, &expiryStub{cfg: oneRule()})

	_, got := postLifecycle(t, h)
	if got.Status != statusOK || got.Deleted != 0 {
		t.Errorf("response = %+v, want a completed sweep of zero", got)
	}
	if got.Reason != "" {
		t.Errorf("reason = %q, want none on a sweep that ran", got.Reason)
	}
}

// TestHandleLifecycle_NoRulesSkips holds the other half: config that never
// reached the process is a different answer from a rule that matched nothing,
// and an operator checking a rule they just wrote needs to know which.
func TestHandleLifecycle_NoRulesSkips(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]*config.LifecycleConfig{
		"no lifecycle block":  nil,
		"block with no rules": {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newCoverageHandler(t)
			stub := &expiryStub{cfg: cfg}
			lifecycleWith(t, h, stub)

			_, got := postLifecycle(t, h)
			if got.Status != statusSkipped || got.Reason == "" {
				t.Errorf("response = %+v, want a skip naming the reason", got)
			}
			if stub.called {
				t.Error("no rules configured; the sweep should not have run")
			}
		})
	}
}

// TestHandleLifecycle_StreamsProgressAndResult is the point of streaming the
// sweep: a large backlog reports what it is expiring as it goes instead of
// holding the operator on one line until the whole thing finishes.
func TestHandleLifecycle_StreamsProgressAndResult(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	lifecycleWith(t, h, &expiryStub{cfg: oneRule(), deleted: 2, failed: 1})

	w := httptest.NewRecorder()
	h.handleLifecycle(w, streamReq("/admin/api/lifecycle"))

	events := decodeEvents(t, w.Body.Bytes())
	if events[0].Kind != adminstream.KindStart || events[0].Op != "lifecycle" {
		t.Errorf("first event = %+v, want start/lifecycle", events[0])
	}
	if !strings.Contains(events[1].Message, "expiring gone-0") {
		t.Errorf("step line = %q, want the object being expired", events[1].Message)
	}
	// Objects are deleted one at a time, so each is a start/end pair.
	stepStarts, steps := countStepEvents(events)
	if stepStarts != 3 || steps != 3 {
		t.Errorf("step events = %d step_start / %d step_end, want 3 / 3", stepStarts, steps)
	}
	last := events[len(events)-1]
	if last.Kind != adminstream.KindResult || last.Outcome != adminstream.OutcomeOK || last.Processed != 2 {
		t.Errorf("last event = %+v, want result/ok with 2 processed", last)
	}
	if last.Fields["failed"] != float64(1) {
		t.Errorf("result fields = %v, want the failure count carried through", last.Fields)
	}
}

// TestHandleLifecycle_StreamsSkip asserts the streaming path keeps the
// distinction the endpoint exists for: no rules configured ends the stream with
// the server's reason, not a completed sweep of zero.
func TestHandleLifecycle_StreamsSkip(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	lifecycleWith(t, h, &expiryStub{})

	w := httptest.NewRecorder()
	h.handleLifecycle(w, streamReq("/admin/api/lifecycle"))

	last := decodeEvents(t, w.Body.Bytes())[1]
	if last.Outcome != adminstream.OutcomeSkipped || last.Message == "" {
		t.Errorf("last event = %+v, want a skip naming its reason", last)
	}
}

// TestHandleLifecycle_UnwiredManagerSkips covers the deployment where the
// expiry manager is absent entirely, which must answer rather than panic.
func TestHandleLifecycle_UnwiredManagerSkips(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	lifecycleWith(t, h, nil)

	w, got := postLifecycle(t, h)
	if w.Code != http.StatusOK || got.Status != statusSkipped {
		t.Errorf("status = %d, response = %+v, want 200 and a skip", w.Code, got)
	}
}
