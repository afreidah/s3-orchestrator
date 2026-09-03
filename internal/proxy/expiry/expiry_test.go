// -------------------------------------------------------------------------------
// Expiry Tests - Lifecycle Rule Evaluation
//
// Author: Alex Freidah
//
// Covers rule application end to end: which objects get deleted, how batches
// page, and the two ways a rule stops early - the store running dry and the
// zero-progress guard that keeps a backend outage from looping forever.
//
// Exercises the manager against a 1-method ExpiredObjectsLister mock and a
// 1-method ObjectDeleter mock rather than standing up a full proxy stack.
// -------------------------------------------------------------------------------

package expiry

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// queryMatcher matches an ExpiredObjectsQuery on the one field a test cares
// about. Matching the whole struct is not an option: the cutoff is derived
// from the clock at call time.
type queryMatcher struct {
	name  string
	match func(core.ExpiredObjectsQuery) bool
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

func (m queryMatcher) Matches(x any) bool {
	q, ok := x.(core.ExpiredObjectsQuery)
	return ok && m.match(q)
}

func (m queryMatcher) String() string { return m.name }

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// queryPrefix matches a query by the prefix its rule carried.
func queryPrefix(p string) queryMatcher {
	return queryMatcher{
		name:  "query with prefix " + p,
		match: func(q core.ExpiredObjectsQuery) bool { return q.Prefix == p },
	}
}

// queryLimit matches a query by its batch size.
func queryLimit(n int) queryMatcher {
	return queryMatcher{
		name:  fmt.Sprintf("query with limit %d", n),
		match: func(q core.ExpiredObjectsQuery) bool { return q.Limit == n },
	}
}

// pages returns a ListExpiredObjects stub that yields each supplied batch in
// turn and then reports the store as exhausted, mirroring how a real cursor
// drains.
func pages(batches ...[]core.ObjectLocation) func(context.Context, core.ExpiredObjectsQuery) ([]core.ObjectLocation, error) {
	var n int
	return func(context.Context, core.ExpiredObjectsQuery) ([]core.ObjectLocation, error) {
		if n >= len(batches) {
			return nil, nil
		}
		batch := batches[n]
		n++
		return batch, nil
	}
}

// objectsNamed builds a batch of expired locations from keys.
func objectsNamed(keys ...string) []core.ObjectLocation {
	out := make([]core.ObjectLocation, 0, len(keys))
	for _, k := range keys {
		out = append(out, core.ObjectLocation{
			ObjectKey: k, BackendName: "b1", SizeBytes: 4,
			CreatedAt: time.Now().Add(-48 * time.Hour),
		})
	}
	return out
}

// newManager wires a Manager over the two mocks and records every deleted key.
func newManager(t *testing.T, ctrl *gomock.Controller) (*Manager, *storetest.MockExpiredObjectsLister, *MockObjectDeleter) {
	t.Helper()
	lister := storetest.NewMockExpiredObjectsLister(ctrl)
	deleter := NewMockObjectDeleter(ctrl)
	return New(lister, deleter, nil), lister, deleter
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestProcessRules_DeletesExpiredObjects drives the happy path.
func TestProcessRules_DeletesExpiredObjects(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m, lister, deleter := newManager(t, ctrl)

	lister.EXPECT().ListExpiredObjects(gomock.Any(), queryPrefix("tmp/")).
		DoAndReturn(pages(objectsNamed("tmp/old-file"))).AnyTimes()
	deleter.EXPECT().DeleteObject(gomock.Any(), "tmp/old-file").Return(nil).Times(1)

	deleted, failed := m.ProcessRules(t.Context(), []config.LifecycleRule{
		{Prefix: "tmp/", ExpirationDays: 1},
	}, nil)
	if deleted != 1 || failed != 0 {
		t.Errorf("deleted=%d failed=%d, want 1/0", deleted, failed)
	}
}

// TestProcessRules_PassesTagFilter verifies a rule's tags reach the store
// query. Without this the filter would be silently dropped and the rule would
// expire every object under its prefix.
func TestProcessRules_PassesTagFilter(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m, lister, deleter := newManager(t, ctrl)

	tags := map[string]string{"env": "staging", "team": "infra"}
	matchTags := queryMatcher{
		name: "query carrying the rule's tags",
		match: func(q core.ExpiredObjectsQuery) bool {
			return maps.Equal(q.Tags, tags)
		},
	}

	lister.EXPECT().ListExpiredObjects(gomock.Any(), matchTags).
		DoAndReturn(pages(objectsNamed("logs/a"))).AnyTimes()
	deleter.EXPECT().DeleteObject(gomock.Any(), "logs/a").Return(nil).Times(1)

	deleted, failed := m.ProcessRules(t.Context(), []config.LifecycleRule{
		{Prefix: "logs/", Tags: tags, ExpirationDays: 1},
	}, nil)
	if deleted != 1 || failed != 0 {
		t.Errorf("deleted=%d failed=%d, want 1/0", deleted, failed)
	}
}

// TestProcessRules_NoExpiredObjects covers the no-op case: nothing expired,
// nothing deleted.
func TestProcessRules_NoExpiredObjects(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m, lister, deleter := newManager(t, ctrl)

	lister.EXPECT().ListExpiredObjects(gomock.Any(), gomock.Any()).
		Return(nil, nil).AnyTimes()
	// Any delete at all would be a bug.
	deleter.EXPECT().DeleteObject(gomock.Any(), gomock.Any()).Times(0)

	deleted, failed := m.ProcessRules(t.Context(), []config.LifecycleRule{
		{Prefix: "tmp/", ExpirationDays: 7},
	}, nil)
	if deleted != 0 || failed != 0 {
		t.Errorf("deleted=%d failed=%d, want 0/0", deleted, failed)
	}
}

// TestProcessRules_MultipleRules confirms each rule runs against its own prefix.
func TestProcessRules_MultipleRules(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m, lister, deleter := newManager(t, ctrl)

	lister.EXPECT().ListExpiredObjects(gomock.Any(), queryPrefix("tmp/")).
		DoAndReturn(pages(objectsNamed("tmp/a"))).AnyTimes()
	lister.EXPECT().ListExpiredObjects(gomock.Any(), queryPrefix("logs/")).
		DoAndReturn(pages(objectsNamed("logs/b"))).AnyTimes()
	deleter.EXPECT().DeleteObject(gomock.Any(), "tmp/a").Return(nil).Times(1)
	deleter.EXPECT().DeleteObject(gomock.Any(), "logs/b").Return(nil).Times(1)

	deleted, failed := m.ProcessRules(t.Context(), []config.LifecycleRule{
		{Prefix: "tmp/", ExpirationDays: 1},
		{Prefix: "logs/", ExpirationDays: 1},
	}, nil)
	if deleted != 2 || failed != 0 {
		t.Errorf("deleted=%d failed=%d, want 2/0", deleted, failed)
	}
}

// TestProcessRules_BatchPagination asserts a full batch triggers another query
// and a short one ends the rule.
func TestProcessRules_BatchPagination(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m, lister, deleter := newManager(t, ctrl)
	m.SetConfig(&config.LifecycleConfig{BatchSize: 2})

	// A full batch of 2, then a short batch of 1, which ends the rule.
	lister.EXPECT().ListExpiredObjects(gomock.Any(), queryLimit(2)).
		DoAndReturn(pages(objectsNamed("a", "b"), objectsNamed("c"))).Times(2)
	deleter.EXPECT().DeleteObject(gomock.Any(), gomock.Any()).Return(nil).Times(3)

	deleted, failed := m.ProcessRules(t.Context(), []config.LifecycleRule{{Prefix: "", ExpirationDays: 1}}, nil)
	if deleted != 3 || failed != 0 {
		t.Errorf("deleted=%d failed=%d, want 3/0", deleted, failed)
	}
}

// TestProcessRules_DeleteFailureContinues asserts one failed delete does not
// abandon the rest of the batch.
func TestProcessRules_DeleteFailureContinues(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m, lister, deleter := newManager(t, ctrl)

	lister.EXPECT().ListExpiredObjects(gomock.Any(), gomock.Any()).
		DoAndReturn(pages(objectsNamed("good", "bad"))).AnyTimes()
	deleter.EXPECT().DeleteObject(gomock.Any(), "good").Return(nil).Times(1)
	deleter.EXPECT().DeleteObject(gomock.Any(), "bad").Return(errors.New("backend down")).Times(1)

	deleted, failed := m.ProcessRules(t.Context(), []config.LifecycleRule{{Prefix: "", ExpirationDays: 1}}, nil)
	if deleted != 1 || failed != 1 {
		t.Errorf("deleted=%d failed=%d, want 1/1", deleted, failed)
	}
}

// TestProcessRules_ListError counts the rule as failed and stops it, rather
// than retrying a store that is not answering.
func TestProcessRules_ListError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m, lister, deleter := newManager(t, ctrl)

	lister.EXPECT().ListExpiredObjects(gomock.Any(), gomock.Any()).
		Return(nil, errors.New("db down")).Times(1)
	deleter.EXPECT().DeleteObject(gomock.Any(), gomock.Any()).Times(0)

	deleted, failed := m.ProcessRules(t.Context(), []config.LifecycleRule{{Prefix: "", ExpirationDays: 1}}, nil)
	if deleted != 0 || failed != 1 {
		t.Errorf("deleted=%d failed=%d, want 0/1", deleted, failed)
	}
}

// TestProcessRules_ZeroProgressTerminates is the infinite-loop guard: when a
// full batch deletes nothing, the rule stops instead of re-listing the same
// rows forever. Without it a backend outage spins until the tick is cancelled.
func TestProcessRules_ZeroProgressTerminates(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m, lister, deleter := newManager(t, ctrl)
	m.SetConfig(&config.LifecycleConfig{BatchSize: 2})

	// The store would happily keep returning full batches forever.
	lister.EXPECT().ListExpiredObjects(gomock.Any(), queryLimit(2)).
		Return(objectsNamed("a", "b"), nil).Times(1)
	deleter.EXPECT().DeleteObject(gomock.Any(), gomock.Any()).
		Return(errors.New("backend down")).Times(2)

	deleted, failed := m.ProcessRules(t.Context(), []config.LifecycleRule{{Prefix: "", ExpirationDays: 1}}, nil)
	if deleted != 0 || failed != 2 {
		t.Errorf("deleted=%d failed=%d, want 0/2", deleted, failed)
	}
}

// TestProcessRules_EmptyRules asserts an empty rule set queries nothing.
func TestProcessRules_EmptyRules(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m, lister, deleter := newManager(t, ctrl)
	lister.EXPECT().ListExpiredObjects(gomock.Any(), gomock.Any()).Times(0)
	deleter.EXPECT().DeleteObject(gomock.Any(), gomock.Any()).Times(0)

	if deleted, failed := m.ProcessRules(t.Context(), nil, nil); deleted != 0 || failed != 0 {
		t.Errorf("deleted=%d failed=%d, want 0/0", deleted, failed)
	}
}

// TestProcessRules_ReportsProgress covers the observer path: every object is
// bracketed with its own start and end step, and the end carries the outcome
// so a watching caller can tell a deleted object from a failed one.
func TestProcessRules_ReportsProgress(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m, lister, deleter := newManager(t, ctrl)

	lister.EXPECT().ListExpiredObjects(gomock.Any(), gomock.Any()).
		DoAndReturn(pages(objectsNamed("good", "bad"))).AnyTimes()
	deleter.EXPECT().DeleteObject(gomock.Any(), "good").Return(nil).Times(1)
	deleter.EXPECT().DeleteObject(gomock.Any(), "bad").Return(errors.New("backend down")).Times(1)

	var steps []progress.Step
	m.ProcessRules(t.Context(), []config.LifecycleRule{{Prefix: "", ExpirationDays: 1}},
		func(s progress.Step) { steps = append(steps, s) })

	want := []progress.Step{
		{Label: "good", Phase: progress.PhaseStart},
		{Label: "good", Phase: progress.PhaseEnd, Status: progress.StatusOK},
		{Label: "bad", Phase: progress.PhaseStart},
		{Label: "bad", Phase: progress.PhaseEnd, Status: progress.StatusFailed},
	}
	if len(steps) != len(want) {
		t.Fatalf("got %d steps, want %d: %+v", len(steps), len(want), steps)
	}
	for i, w := range want {
		if steps[i].Label != w.Label || steps[i].Phase != w.Phase || steps[i].Status != w.Status {
			t.Errorf("step %d = %+v, want %+v", i, steps[i], w)
		}
	}
}

// TestBatchSizeFor covers the configured/unset/non-positive cases of the
// per-tick batch bound.
func TestBatchSizeFor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  *config.LifecycleConfig
		want int
	}{
		{"nil config", nil, defaultBatchSize},
		{"unset", &config.LifecycleConfig{}, defaultBatchSize},
		{"non-positive", &config.LifecycleConfig{BatchSize: -1}, defaultBatchSize},
		{"configured", &config.LifecycleConfig{BatchSize: 25}, 25},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := batchSizeFor(c.cfg); got != c.want {
				t.Errorf("batchSizeFor(%+v) = %d, want %d", c.cfg, got, c.want)
			}
		})
	}
}

// TestSetConfig_RoundTrip covers the reloadable config the manager owns.
func TestSetConfig_RoundTrip(t *testing.T) {
	t.Parallel()
	m := New(nil, nil, nil)
	if m.Config() != nil {
		t.Error("a fresh manager should report no config")
	}
	cfg := &config.LifecycleConfig{BatchSize: 7}
	m.SetConfig(cfg)
	if got := m.Config(); got == nil || got.BatchSize != 7 {
		t.Errorf("Config() = %+v, want BatchSize 7", got)
	}
}
