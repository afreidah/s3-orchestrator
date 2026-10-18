// -------------------------------------------------------------------------------
// Cleanup Settler Tests
//
// Author: Alex Freidah
//
// Drives the settler through fakes of its two collaborators, so each row
// outcome can be asserted directly: retired, retired as already absent,
// retired on an unknown backend, retried with backoff, or moved to the DLQ.
// The fakes also record the context each outcome was written on.
// -------------------------------------------------------------------------------

package cleanup

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// FAKES
// -------------------------------------------------------------------------

// fakeDeleter knows a fixed set of backends and fails the paths named in
// errs. It records each DeleteMany call by backend.
type fakeDeleter struct {
	mu       sync.Mutex
	known    map[string]bool
	errs     map[string]error
	calls    map[string][][]string
	onDelete func()
}

// newFakeDeleter returns a deleter that knows the named backends.
func newFakeDeleter(known ...string) *fakeDeleter {
	d := &fakeDeleter{known: map[string]bool{}, errs: map[string]error{}, calls: map[string][][]string{}}
	for _, name := range known {
		d.known[name] = true
	}
	return d
}

// GetBackend reports an error for any backend the fake does not know.
func (d *fakeDeleter) GetBackend(name string) (backend.ObjectBackend, error) {
	if !d.known[name] {
		return nil, errors.New("backend not registered")
	}
	return backendtest.NewInMemory(), nil
}

// DeleteMany records the call and fails the paths the test named.
func (d *fakeDeleter) DeleteMany(_ context.Context, name string, _ backend.ObjectBackend, keys []string) map[string]error {
	if d.onDelete != nil {
		d.onDelete()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls[name] = append(d.calls[name], slices.Clone(keys))
	failed := map[string]error{}
	for _, k := range keys {
		if err, ok := d.errs[k]; ok {
			failed[k] = err
		}
	}
	return failed
}

// retryCall is one RetryCleanupItem call.
type retryCall struct {
	backoff   time.Duration
	lastError string
}

// fakeStore records every row outcome and the context error at write time.
// The err fields fail the matching call; dlqMoved is what MoveCleanupToDLQ
// reports on success.
type fakeStore struct {
	mu          sync.Mutex
	completed   []int64
	retried     map[int64]retryCall
	dlq         []int64
	ctxErrs     []error
	completeErr error
	retryErr    error
	dlqErr      error
	dlqMoved    bool
}

// newFakeStore returns a store whose DLQ moves succeed.
func newFakeStore() *fakeStore {
	return &fakeStore{retried: map[int64]retryCall{}, dlqMoved: true}
}

// CompleteCleanupItem records the retired row.
func (s *fakeStore) CompleteCleanupItem(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completed = append(s.completed, id)
	s.ctxErrs = append(s.ctxErrs, ctx.Err())
	return s.completeErr
}

// RetryCleanupItem records the retried row with its backoff.
func (s *fakeStore) RetryCleanupItem(ctx context.Context, id int64, backoff time.Duration, lastError string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retried[id] = retryCall{backoff: backoff, lastError: lastError}
	s.ctxErrs = append(s.ctxErrs, ctx.Err())
	return s.retryErr
}

// MoveCleanupToDLQ records the graduated row.
func (s *fakeStore) MoveCleanupToDLQ(ctx context.Context, id int64, _ string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dlq = append(s.dlq, id)
	s.ctxErrs = append(s.ctxErrs, ctx.Err())
	return s.dlqMoved, s.dlqErr
}

// sortedCompleted returns the retired row ids in ascending order.
func (s *fakeStore) sortedCompleted() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Clone(s.completed)
	slices.Sort(out)
	return out
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// row builds one queued cleanup row.
func row(id int64, backendName, path string, attempts int32) core.CleanupItem {
	return core.CleanupItem{
		ID: id, BackendName: backendName, ObjectKey: "k", StorageKey: path,
		Reason: core.CleanupReasonDeleted, Attempts: attempts, SizeBytes: 4,
	}
}

// processed reads CleanupQueueProcessedTotal for status.
func processed(status string) float64 {
	return promtest.ToFloat64(telemetry.CleanupQueueProcessedTotal.WithLabelValues(status))
}

// recordAudit captures every audit event emitted until the test ends.
func recordAudit(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var events []string
	audit.SetOnEvent(func(e string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
	})
	t.Cleanup(func() { audit.SetOnEvent(nil) })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(events)
	}
}

// The backends the tests delete from.
const (
	beA = "a"
	beB = "b"
)

// errDelete is the backend failure the retry and DLQ tests inject.
var errDelete = errors.New("connection refused")

// -------------------------------------------------------------------------
// BACKOFF
// -------------------------------------------------------------------------

// TestBackoff pins the doubling from one minute, the 24h cap, and the
// out-of-range inputs that short-circuit to the cap.
func TestBackoff(t *testing.T) {
	t.Parallel()
	cases := []struct {
		attempts int32
		want     time.Duration
	}{
		{-1, 24 * time.Hour},
		{0, time.Minute},
		{3, 8 * time.Minute},
		{10, 1024 * time.Minute},
		{11, 24 * time.Hour},
		{100, 24 * time.Hour},
	}
	for _, tc := range cases {
		if got := Backoff(tc.attempts); got != tc.want {
			t.Errorf("Backoff(%d) = %v, want %v", tc.attempts, got, tc.want)
		}
	}
}

// -------------------------------------------------------------------------
// SETTLE
// -------------------------------------------------------------------------

// TestSettle_Empty verifies an empty batch deletes nothing and writes nothing.
func TestSettle_Empty(t *testing.T) {
	t.Parallel()
	d, s := newFakeDeleter(beA), newFakeStore()
	sum := New(Deps{Deleter: d, Store: s}).Settle(context.Background(), nil)
	if sum.Planned != 0 || len(d.calls) != 0 || len(s.completed) != 0 {
		t.Errorf("summary %+v, %d delete calls, %d completions; want nothing", sum, len(d.calls), len(s.completed))
	}
}

// TestSettle_GivesEachRowItsOutcome runs one batch holding every outcome and
// asserts each row's store write, the status counters, and the audit trail.
func TestSettle_GivesEachRowItsOutcome(t *testing.T) {
	events := recordAudit(t)
	d, s := newFakeDeleter(beA, beB), newFakeStore()
	d.errs["k!absent"] = &backendtest.StatusError{Code: 404, Msg: "NoSuchKey"}
	d.errs["k!retry"] = errDelete
	d.errs["k!exhausted"] = errDelete

	before := map[string]float64{}
	for _, st := range []string{statusSuccess, statusAbsent, statusRetry, statusExhausted} {
		before[st] = processed(st)
	}
	sum := New(Deps{Deleter: d, Store: s, Concurrency: 4, Name: "settle-test"}).Settle(context.Background(), []core.CleanupItem{
		row(1, beA, "k!gone", 0),
		row(2, beA, "k!absent", 0),
		row(3, beA, "k!retry", 2),
		row(4, beA, "k!exhausted", maxAttempts-1),
		row(5, "retired", "k!unknown", 0),
		row(6, beB, "", 0),
	})

	if sum.Succeeded != 4 || sum.Failed != 2 {
		t.Errorf("summary = %+v, want 4 succeeded and 2 failed", sum)
	}
	if got := s.sortedCompleted(); !slices.Equal(got, []int64{1, 2, 5, 6}) {
		t.Errorf("completed %v, want rows 1, 2, 5 and 6", got)
	}
	if rc, ok := s.retried[3]; !ok || rc.backoff != Backoff(2) || rc.lastError != errDelete.Error() {
		t.Errorf("retry of row 3 = %+v (found %v), want Backoff(2) with the delete error", rc, ok)
	}
	if !slices.Equal(s.dlq, []int64{4}) {
		t.Errorf("dlq %v, want row 4", s.dlq)
	}
	assertGrouped(t, d)
	assertStatusDeltas(t, before, map[string]float64{statusSuccess: 3, statusAbsent: 1, statusRetry: 1, statusExhausted: 1})
	for _, want := range []string{"cleanup_queue.processed", "cleanup_queue.already_absent", "cleanup_queue.exhausted_to_dlq"} {
		if !slices.Contains(events(), want) {
			t.Errorf("audit events %v, want %s", events(), want)
		}
	}
}

// assertGrouped checks each known backend was deleted from in one batch, an
// empty storage key falling back to the object key.
func assertGrouped(t *testing.T, d *fakeDeleter) {
	t.Helper()
	if len(d.calls[beA]) != 1 || len(d.calls[beA][0]) != 4 {
		t.Errorf("backend a calls = %v, want one batch of its 4 paths", d.calls[beA])
	}
	if len(d.calls[beB]) != 1 || !slices.Equal(d.calls[beB][0], []string{"k"}) {
		t.Errorf("backend b calls = %v, want one batch naming the object key", d.calls[beB])
	}
	if _, ok := d.calls["retired"]; ok {
		t.Error("deleted from a backend that is not registered")
	}
}

// assertStatusDeltas checks each status counter moved by exactly want.
func assertStatusDeltas(t *testing.T, before, want map[string]float64) {
	t.Helper()
	for st, delta := range want {
		if got := processed(st) - before[st]; got != delta {
			t.Errorf("processed{%s} delta = %v, want %v", st, got, delta)
		}
	}
}

// TestSettle_ExhaustedPublishesAndCounts verifies a row out of retries is
// counted against its backend's DLQ metric and published as an event.
func TestSettle_ExhaustedPublishesAndCounts(t *testing.T) {
	const dlqBackend = "dlq-published"
	var mu sync.Mutex
	var published []event.Event
	event.SetEmitter(func(ev event.Event) {
		mu.Lock()
		defer mu.Unlock()
		published = append(published, ev)
	})
	t.Cleanup(func() { event.SetEmitter(nil) })

	d, s := newFakeDeleter(dlqBackend), newFakeStore()
	d.errs["k!x"] = errDelete
	before := promtest.ToFloat64(telemetry.CleanupDLQEnqueuedTotal.WithLabelValues(dlqBackend))
	New(Deps{Deleter: d, Store: s}).Settle(context.Background(), []core.CleanupItem{row(1, dlqBackend, "k!x", maxAttempts)})

	if got := promtest.ToFloat64(telemetry.CleanupDLQEnqueuedTotal.WithLabelValues(dlqBackend)) - before; got != 1 {
		t.Errorf("dlq enqueued delta = %v, want 1", got)
	}
	if len(published) != 1 || published[0].Type != event.CleanupExhausted || published[0].Subject != dlqBackend {
		t.Errorf("published %+v, want one CleanupExhausted for %s", published, dlqBackend)
	}
}

// TestSettle_DLQMoveNotCounted verifies a DLQ move that failed, or that found
// the row already gone, is not counted as a graduation.
func TestSettle_DLQMoveNotCounted(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		backend string
		moved   bool
		err     error
	}{
		{"move failed", "dlq-failed", false, errors.New("db down")},
		{"row already gone", "dlq-gone", false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, s := newFakeDeleter(tc.backend), newFakeStore()
			d.errs["k!x"] = errDelete
			s.dlqMoved, s.dlqErr = tc.moved, tc.err
			before := promtest.ToFloat64(telemetry.CleanupDLQEnqueuedTotal.WithLabelValues(tc.backend))
			New(Deps{Deleter: d, Store: s}).Settle(context.Background(), []core.CleanupItem{row(1, tc.backend, "k!x", maxAttempts-1)})

			if len(s.dlq) != 1 {
				t.Fatalf("dlq moves = %v, want one attempt", s.dlq)
			}
			if got := promtest.ToFloat64(telemetry.CleanupDLQEnqueuedTotal.WithLabelValues(tc.backend)) - before; got != 0 {
				t.Errorf("dlq enqueued delta = %v, want 0", got)
			}
		})
	}
}

// TestSettle_StoreWriteFailuresAreLogged verifies a failed completion or retry
// write does not stop the batch: every row still gets its write attempted.
func TestSettle_StoreWriteFailuresAreLogged(t *testing.T) {
	t.Parallel()
	d, s := newFakeDeleter(beA), newFakeStore()
	d.errs["k!retry"] = errDelete
	s.completeErr, s.retryErr = errors.New("db down"), errors.New("db down")

	sum := New(Deps{Deleter: d, Store: s}).Settle(context.Background(), []core.CleanupItem{
		row(1, beA, "k!gone", 0),
		row(2, beA, "k!retry", 0),
	})
	if len(s.completed) != 1 || len(s.retried) != 1 {
		t.Errorf("completed %v, retried %v; want one write each", s.completed, s.retried)
	}
	if sum.Succeeded != 1 || sum.Failed != 1 {
		t.Errorf("summary = %+v, want 1 succeeded and 1 failed", sum)
	}
}

// TestSettle_AdmissionWrapsEachBackend verifies an admission hook that runs
// the delete is called once per backend and every row is settled.
func TestSettle_AdmissionWrapsEachBackend(t *testing.T) {
	t.Parallel()
	d, s := newFakeDeleter(beA, beB), newFakeStore()
	var mu sync.Mutex
	admitted := 0
	admit := func(_ context.Context, fn func()) {
		mu.Lock()
		admitted++
		mu.Unlock()
		fn()
	}
	New(Deps{Deleter: d, Store: s, Admit: admit, Concurrency: 2}).Settle(context.Background(), []core.CleanupItem{
		row(1, beA, "k!1", 0), row(2, beA, "k!2", 0), row(3, beB, "k!3", 0),
	})
	if admitted != 2 {
		t.Errorf("admitted %d times, want once per backend", admitted)
	}
	if got := s.sortedCompleted(); !slices.Equal(got, []int64{1, 2, 3}) {
		t.Errorf("completed %v, want every row", got)
	}
}

// TestSettle_DeclinedAdmissionLeavesRowsUnsettled verifies a backend whose
// admission was declined is not deleted from and its rows get no outcome, so
// they wait for a later attempt.
func TestSettle_DeclinedAdmissionLeavesRowsUnsettled(t *testing.T) {
	t.Parallel()
	d, s := newFakeDeleter(beA), newFakeStore()
	decline := func(context.Context, func()) {}
	sum := New(Deps{Deleter: d, Store: s, Admit: decline}).Settle(context.Background(), []core.CleanupItem{
		row(1, beA, "k!1", 0), row(2, beA, "k!2", 0),
	})
	if len(d.calls) != 0 {
		t.Errorf("delete calls %v, want none under a declined admission", d.calls)
	}
	if len(s.completed)+len(s.retried)+len(s.dlq) != 0 {
		t.Error("a row whose admission was declined was given an outcome")
	}
	if sum.Skipped != 2 {
		t.Errorf("summary = %+v, want both rows skipped", sum)
	}
}

// TestSettle_OutcomesSurviveCancellation verifies a caller cancelling after its
// deletes ran still gets every row's outcome written, on a context the
// cancellation does not reach.
func TestSettle_OutcomesSurviveCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, s := newFakeDeleter(beA), newFakeStore()
	d.errs["k!retry"] = errDelete
	d.onDelete = cancel

	New(Deps{Deleter: d, Store: s}).Settle(ctx, []core.CleanupItem{row(1, beA, "k!gone", 0), row(2, beA, "k!retry", 0)})

	if len(s.completed) != 1 || len(s.retried) != 1 {
		t.Fatalf("completed %v, retried %v; want both outcomes written", s.completed, s.retried)
	}
	for _, err := range s.ctxErrs {
		if err != nil {
			t.Errorf("an outcome was written on a cancelled context: %v", err)
		}
	}
}
