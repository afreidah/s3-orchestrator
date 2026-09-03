// -------------------------------------------------------------------------------
// Ops - Lifecycle Operation Tests
//
// Author: Alex Freidah
//
// The on-demand expiration sweep. Its whole purpose is telling three states
// apart - the rule matched and deleted, the rule ran and matched nothing, and
// there is no rule at all - so each is pinned here, along with the event a
// completed sweep publishes.
// -------------------------------------------------------------------------------

package ops

import (
	"context"
	"errors"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/progress"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// lifecycleStub stands in for *expiry.Manager, reporting a fixed outcome and
// recording the rules it was handed.
type lifecycleStub struct {
	cfg         *config.LifecycleConfig
	deleted     int
	failed      int
	gotRules    []config.LifecycleRule
	gotObserver progress.Observer
	calls       int
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Config returns the configured rules, or nil for a deployment with none.
func (s *lifecycleStub) Config() *config.LifecycleConfig { return s.cfg }

// ProcessRules records the call and reports the fixed outcome.
func (s *lifecycleStub) ProcessRules(_ context.Context, rules []config.LifecycleRule, obs progress.Observer) (int, int) {
	s.gotObserver = obs
	s.calls++
	s.gotRules = rules
	return s.deleted, s.failed
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// rules is a minimal configured ruleset.
func rules() *config.LifecycleConfig {
	return &config.LifecycleConfig{Rules: []config.LifecycleRule{{Prefix: "tmp/", ExpirationDays: 7}}}
}

// captureEvents installs an emitter for one test and returns the slice it fills.
// Not parallel-safe: the emitter is process-global.
func captureEvents(t *testing.T) *[]event.Event {
	t.Helper()
	var got []event.Event
	event.SetEmitter(func(ev event.Event) { got = append(got, ev) })
	t.Cleanup(func() { event.SetEmitter(nil) })
	return &got
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestLifecycleRun_ReportsWhatItDeleted covers the answer an operator is after,
// and that the configured rules are what actually reach the sweep.
//
// Not parallel: asserts on the process-global event emitter.
func TestLifecycleRun_ReportsWhatItDeleted(t *testing.T) {
	emitted := captureEvents(t)
	stub := &lifecycleStub{cfg: rules(), deleted: 12, failed: 2}

	res, err := NewLifecycle(LifecycleDeps{Expiry: stub}).Run(context.Background(), func(progress.Step) {})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The observer is the whole point of a streamed sweep; dropping it here
	// would leave the caller watching a run that reports nothing.
	if stub.gotObserver == nil {
		t.Error("the caller's observer did not reach ProcessRules")
	}
	if res.Deleted != 12 || res.Failed != 2 {
		t.Errorf("res = %+v, want 12 deleted / 2 failed", res)
	}
	if stub.calls != 1 {
		t.Errorf("ProcessRules called %d times, want 1", stub.calls)
	}
	if len(stub.gotRules) != 1 || stub.gotRules[0].Prefix != "tmp/" {
		t.Errorf("rules = %+v, want the configured ruleset", stub.gotRules)
	}

	// The scheduled tick publishes lifecycle.completed; a manual sweep is the
	// same work and has to look the same to anything subscribed.
	if len(*emitted) != 1 || (*emitted)[0].Type != event.LifecycleCompleted {
		t.Fatalf("emitted = %+v, want one %s event", *emitted, event.LifecycleCompleted)
	}
	if (*emitted)[0].Data["deleted"] != 12 || (*emitted)[0].Data["failed"] != 2 {
		t.Errorf("event data = %v, want the sweep's counts", (*emitted)[0].Data)
	}
}

// TestLifecycleRun_MatchedNothingIsNotASkip is the distinction the operation
// exists for: a rule that ran and matched nothing completed, and reporting it
// as skipped would read as "nothing happened" when something did.
func TestLifecycleRun_MatchedNothingIsNotASkip(t *testing.T) {
	res, err := NewLifecycle(LifecycleDeps{Expiry: &lifecycleStub{cfg: rules()}}).Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("a sweep that matched nothing is not a skip, got %v", err)
	}
	if res.Deleted != 0 || res.Failed != 0 {
		t.Errorf("res = %+v, want a completed sweep of zero", res)
	}
}

// TestLifecycleRun_NoRulesSkips holds the other half: config that never reached
// the process is a different answer from a rule that matched nothing. Both the
// absent block and an empty ruleset mean there is nothing to run.
func TestLifecycleRun_NoRulesSkips(t *testing.T) {
	for name, cfg := range map[string]*config.LifecycleConfig{
		"no lifecycle block":  nil,
		"block with no rules": {},
	} {
		t.Run(name, func(t *testing.T) {
			stub := &lifecycleStub{cfg: cfg}
			_, err := NewLifecycle(LifecycleDeps{Expiry: stub}).Run(context.Background(), nil)

			var skip *SkipError
			if !errors.As(err, &skip) {
				t.Fatalf("err = %v, want a SkipError", err)
			}
			if skip.Reason == "" {
				t.Error("a skip must name its reason; the operator is asking why nothing ran")
			}
			if stub.calls != 0 {
				t.Error("no rules configured; the sweep should not have run")
			}
		})
	}
}

// TestLifecycleRun_UnwiredManagerSkips covers the deployment where the expiry
// manager is absent entirely, which must report that rather than panic.
func TestLifecycleRun_UnwiredManagerSkips(t *testing.T) {
	_, err := NewLifecycle(LifecycleDeps{}).Run(context.Background(), nil)
	if !errors.Is(err, ErrLifecycleUnavailable) {
		t.Errorf("err = %v, want ErrLifecycleUnavailable", err)
	}
}
