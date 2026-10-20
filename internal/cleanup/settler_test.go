// -------------------------------------------------------------------------------
// Cleanup Settler Tests
//
// Author: Alex Freidah
//
// Drives the settler through fakes of its two collaborators, so each row
// outcome can be asserted directly: retired, retired as already absent,
// retired on an unknown backend, retried with backoff, moved to the DLQ, or
// left alone because another instance took over its claim. The fakes also
// record the claim and context each outcome was written with.
// -------------------------------------------------------------------------------

package cleanup

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
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
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
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

// newFakeDeleter returns a deleter that knows the named backends and fails
// pathFail with errDelete.
func newFakeDeleter(known ...string) *fakeDeleter {
	d := &fakeDeleter{
		known: map[string]bool{},
		errs:  map[string]error{pathFail: errDelete},
		calls: map[string][][]string{},
	}
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

// fakeStore records every row outcome, the claims and the context error at
// write time. The err fields fail the matching call. A row named in lost has
// had its claim taken over: CompleteCleanupItems leaves it out of the retired
// ids, and RetryCleanupItem and MoveCleanupToDLQ report false.
type fakeStore struct {
	mu            sync.Mutex
	completeCalls [][]core.CleanupItem
	retired       []int64
	retried       map[int64]retryCall
	dlq           []int64
	claims        []*core.CleanupClaim
	ctxErrs       []error
	completeErr   error
	retryErr      error
	dlqErr        error
	lost          map[int64]bool
}

// newFakeStore returns a store whose writes all succeed.
func newFakeStore() *fakeStore {
	return &fakeStore{retried: map[int64]retryCall{}, lost: map[int64]bool{}}
}

// CompleteCleanupItems records the call and retires every row not lost.
func (s *fakeStore) CompleteCleanupItems(ctx context.Context, items []core.CleanupItem) ([]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completeCalls = append(s.completeCalls, slices.Clone(items))
	s.ctxErrs = append(s.ctxErrs, ctx.Err())
	for i := range items {
		s.claims = append(s.claims, items[i].Claim)
	}
	if s.completeErr != nil {
		return nil, s.completeErr
	}
	var ids []int64
	for i := range items {
		if !s.lost[items[i].ID] {
			ids = append(ids, items[i].ID)
		}
	}
	s.retired = append(s.retired, ids...)
	return ids, nil
}

// RetryCleanupItem records the retried row with its backoff.
func (s *fakeStore) RetryCleanupItem(ctx context.Context, item *core.CleanupItem, backoff time.Duration, lastError string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retried[item.ID] = retryCall{backoff: backoff, lastError: lastError}
	s.claims = append(s.claims, item.Claim)
	s.ctxErrs = append(s.ctxErrs, ctx.Err())
	return !s.lost[item.ID], s.retryErr
}

// MoveCleanupToDLQ records the graduated row.
func (s *fakeStore) MoveCleanupToDLQ(ctx context.Context, item *core.CleanupItem, _ string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dlq = append(s.dlq, item.ID)
	s.claims = append(s.claims, item.Claim)
	s.ctxErrs = append(s.ctxErrs, ctx.Err())
	return !s.lost[item.ID], s.dlqErr
}

// sortedRetired returns the retired row ids in ascending order.
func (s *fakeStore) sortedRetired() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Clone(s.retired)
	slices.Sort(out)
	return out
}

// writes counts every store call the settler made.
func (s *fakeStore) writes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.completeCalls) + len(s.retried) + len(s.dlq)
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// The backends the tests delete from; beGone is not registered.
const (
	beA    = "a"
	beB    = "b"
	beGone = "retired"
)

// The storage paths the tests delete: pathOK deletes, pathAbsent answers 404,
// and pathFail fails with errDelete.
const (
	pathOK     = "k!gone"
	pathAbsent = "k!absent"
	pathFail   = "k!fail"
)

// The audit events a settled row records.
const (
	auditProcessed = "cleanup_queue.processed"
	auditAbsent    = "cleanup_queue.already_absent"
	auditDLQ       = "cleanup_queue.exhausted_to_dlq"
)

// errDelete is the backend failure the retry and DLQ tests inject.
var errDelete = errors.New("connection refused")

// errStore is the store failure the write-error tests inject.
var errStore = errors.New("db down")

// testClaim is the claim every test row was read under.
var testClaim = core.CleanupClaim{At: time.Unix(1_700_000_000, 0), By: "instance-1"}

// allStatuses lists every status a settled row can report.
var allStatuses = []string{statusSuccess, statusAbsent, statusRetry, statusExhausted, statusClaimLost}

// row builds one queued cleanup row held under testClaim.
func row(id int64, backendName, path string, attempts int32) core.CleanupItem {
	claim := testClaim
	return core.CleanupItem{
		ID: id, BackendName: backendName, ObjectKey: "k", StorageKey: path,
		Reason: core.CleanupReasonDeleted, Attempts: attempts, SizeBytes: 4,
		Claim: &claim,
	}
}

// settleWith builds a settler over d and s and settles items.
func settleWith(d *fakeDeleter, s *fakeStore, items ...core.CleanupItem) batch.Summary {
	return New(Deps{Deleter: d, Store: s}).Settle(context.Background(), items)
}

// processed reads CleanupQueueProcessedTotal for status.
func processed(status string) float64 {
	return promtest.ToFloat64(telemetry.CleanupQueueProcessedTotal.WithLabelValues(status))
}

// dlqEnqueued reads CleanupDLQEnqueuedTotal for backendName.
func dlqEnqueued(backendName string) float64 {
	return promtest.ToFloat64(telemetry.CleanupDLQEnqueuedTotal.WithLabelValues(backendName))
}

// statusCounts snapshots CleanupQueueProcessedTotal for every status.
func statusCounts() map[string]float64 {
	out := map[string]float64{}
	for _, st := range allStatuses {
		out[st] = processed(st)
	}
	return out
}

// assertStatusDeltas checks every status counter moved by exactly its entry in
// want, a missing entry meaning it did not move.
func assertStatusDeltas(t *testing.T, before, want map[string]float64) {
	t.Helper()
	for _, st := range allStatuses {
		if got := processed(st) - before[st]; got != want[st] {
			t.Errorf("processed{%s} delta = %v, want %v", st, got, want[st])
		}
	}
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

// recordEvents captures every published event until the test ends.
func recordEvents(t *testing.T) func() []event.Event {
	t.Helper()
	var mu sync.Mutex
	var published []event.Event
	event.SetEmitter(func(ev event.Event) {
		mu.Lock()
		defer mu.Unlock()
		published = append(published, ev)
	})
	t.Cleanup(func() { event.SetEmitter(nil) })
	return func() []event.Event {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(published)
	}
}

// -------------------------------------------------------------------------
// BACKOFF
// -------------------------------------------------------------------------

// TestRetryBackoff pins the cleanup queue's policy: doubling from one minute,
// capped at a day.
func TestRetryBackoff(t *testing.T) {
	t.Parallel()
	for attempts, want := range map[int32]time.Duration{0: time.Minute, 10: 1024 * time.Minute, 11: 24 * time.Hour} {
		if got := RetryBackoff.For(attempts); got != want {
			t.Errorf("RetryBackoff.For(%d) = %v, want %v", attempts, got, want)
		}
	}
}

// -------------------------------------------------------------------------
// SETTLE
// -------------------------------------------------------------------------

// TestSettle_Empty verifies an empty batch deletes nothing and writes
// nothing.
func TestSettle_Empty(t *testing.T) {
	t.Parallel()
	d, s := newFakeDeleter(beA), newFakeStore()
	sum := settleWith(d, s)
	if sum.Planned != 0 || len(d.calls) != 0 || s.writes() != 0 {
		t.Errorf("summary %+v, %d delete calls, %d writes; want nothing", sum, len(d.calls), s.writes())
	}
}

// TestSettle_GivesEachRowItsOutcome runs one batch holding every outcome and
// asserts each row's store write, the summary, the counters and the audit.
func TestSettle_GivesEachRowItsOutcome(t *testing.T) {
	events := recordAudit(t)
	d, s := newFakeDeleter(beA, beB), newFakeStore()
	d.errs[pathAbsent] = &backendtest.StatusError{Code: 404, Msg: "NoSuchKey"}
	d.errs["k!exhausted"] = errDelete

	before := statusCounts()
	sum := New(Deps{Deleter: d, Store: s, Concurrency: 4, Name: "settle-test"}).Settle(context.Background(), []core.CleanupItem{
		row(1, beA, pathOK, 0),
		row(2, beA, pathAbsent, 0),
		row(3, beA, pathFail, 2),
		row(4, beA, "k!exhausted", core.MaxCleanupAttempts-1),
		row(5, beGone, "k!unknown", 0),
		row(6, beB, "", 0),
	})

	want := batch.Summary{Planned: 6, Attempted: 6, Succeeded: 4, Failed: 2}
	if sum.Planned != want.Planned || sum.Attempted != want.Attempted || sum.Succeeded != want.Succeeded || sum.Failed != want.Failed || sum.Skipped != 0 {
		t.Errorf("summary = %+v, want %+v", sum, want)
	}
	assertOneRetireCall(t, s, []int64{1, 2, 5, 6})
	if rc, ok := s.retried[3]; !ok || rc.backoff != RetryBackoff.For(2) || rc.lastError != errDelete.Error() {
		t.Errorf("retry of row 3 = %+v (found %v), want RetryBackoff.For(2) with the delete error", rc, ok)
	}
	if !slices.Equal(s.dlq, []int64{4}) {
		t.Errorf("dlq %v, want row 4", s.dlq)
	}
	assertClaimsPassed(t, s)
	assertGrouped(t, d)
	assertStatusDeltas(t, before, map[string]float64{statusSuccess: 3, statusAbsent: 1, statusRetry: 1, statusExhausted: 1})
	assertAudit(t, events(), map[string]int{auditProcessed: 2, auditAbsent: 1, auditDLQ: 1})
}

// assertOneRetireCall checks every gone row was retired in a single store
// call holding exactly the rows in want.
func assertOneRetireCall(t *testing.T, s *fakeStore, want []int64) {
	t.Helper()
	if len(s.completeCalls) != 1 {
		t.Fatalf("CompleteCleanupItems called %d times, want once", len(s.completeCalls))
	}
	var ids []int64
	for i := range s.completeCalls[0] {
		ids = append(ids, s.completeCalls[0][i].ID)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, want) {
		t.Errorf("retire call held rows %v, want %v", ids, want)
	}
	if got := s.sortedRetired(); !slices.Equal(got, want) {
		t.Errorf("retired %v, want %v", got, want)
	}
}

// assertClaimsPassed checks every store write carried the claim its row was
// read under.
func assertClaimsPassed(t *testing.T, s *fakeStore) {
	t.Helper()
	for _, c := range s.claims {
		if c == nil || *c != testClaim {
			t.Errorf("store write carried claim %+v, want %+v", c, testClaim)
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
	if _, ok := d.calls[beGone]; ok {
		t.Error("deleted from a backend that is not registered")
	}
}

// assertAudit checks each audit event in want was emitted exactly that many
// times.
func assertAudit(t *testing.T, got []string, want map[string]int) {
	t.Helper()
	for ev, n := range want {
		count := 0
		for _, e := range got {
			if e == ev {
				count++
			}
		}
		if count != n {
			t.Errorf("audit %s emitted %d times, want %d (events %v)", ev, count, n, got)
		}
	}
}

// TestSettle_UnknownBackendRetiresWithoutAudit verifies a row on a backend
// that is no longer registered is retired as a success with no audit entry.
func TestSettle_UnknownBackendRetiresWithoutAudit(t *testing.T) {
	events := recordAudit(t)
	d, s := newFakeDeleter(), newFakeStore()
	before := statusCounts()
	sum := settleWith(d, s, row(1, beGone, pathOK, 0))

	if sum.Succeeded != 1 {
		t.Errorf("summary = %+v, want 1 succeeded", sum)
	}
	assertOneRetireCall(t, s, []int64{1})
	assertStatusDeltas(t, before, map[string]float64{statusSuccess: 1})
	if len(events()) != 0 {
		t.Errorf("audit events %v, want none", events())
	}
}

// TestSettle_AbsentRetiresAsAbsent verifies a 404 from the backend retires the
// row under the absent status and audit event.
func TestSettle_AbsentRetiresAsAbsent(t *testing.T) {
	events := recordAudit(t)
	d, s := newFakeDeleter(beA), newFakeStore()
	d.errs[pathAbsent] = &backendtest.StatusError{Code: 404, Msg: "NoSuchKey"}
	before := statusCounts()
	sum := settleWith(d, s, row(1, beA, pathAbsent, 0))

	if sum.Succeeded != 1 {
		t.Errorf("summary = %+v, want 1 succeeded", sum)
	}
	assertOneRetireCall(t, s, []int64{1})
	assertStatusDeltas(t, before, map[string]float64{statusAbsent: 1})
	assertAudit(t, events(), map[string]int{auditAbsent: 1, auditProcessed: 0})
}

// TestSettle_RetryCountsOnce verifies a failed delete within its budget is
// retried, counted under retry and reported as failed.
func TestSettle_RetryCountsOnce(t *testing.T) {
	d, s := newFakeDeleter(beA), newFakeStore()
	before := statusCounts()
	sum := settleWith(d, s, row(1, beA, pathFail, 0))

	if sum.Failed != 1 || len(s.retried) != 1 {
		t.Errorf("summary = %+v, retried %v; want one failed retry", sum, s.retried)
	}
	assertStatusDeltas(t, before, map[string]float64{statusRetry: 1})
}

// TestSettle_ExhaustedPublishesAndCounts verifies a row out of retries is
// moved to the DLQ, counted, audited and published as an event.
func TestSettle_ExhaustedPublishesAndCounts(t *testing.T) {
	const dlqBackend = "dlq-published"
	published := recordEvents(t)
	events := recordAudit(t)
	d, s := newFakeDeleter(dlqBackend), newFakeStore()
	before, beforeDLQ := statusCounts(), dlqEnqueued(dlqBackend)
	sum := settleWith(d, s, row(1, dlqBackend, pathFail, core.MaxCleanupAttempts))

	if sum.Failed != 1 || !slices.Equal(s.dlq, []int64{1}) {
		t.Errorf("summary = %+v, dlq %v; want one failed DLQ move", sum, s.dlq)
	}
	if got := dlqEnqueued(dlqBackend) - beforeDLQ; got != 1 {
		t.Errorf("dlq enqueued delta = %v, want 1", got)
	}
	assertStatusDeltas(t, before, map[string]float64{statusExhausted: 1})
	assertAudit(t, events(), map[string]int{auditDLQ: 1})
	evs := published()
	if len(evs) != 1 || evs[0].Type != event.CleanupExhausted || evs[0].Subject != dlqBackend {
		t.Errorf("published %+v, want one CleanupExhausted for %s", evs, dlqBackend)
	}
}

// TestSettle_LostClaimLeavesRow verifies a row whose claim another instance
// took over, at the retire, the retry or the DLQ move, is counted as lost and
// skipped while the rest of the batch settles normally.
func TestSettle_LostClaimLeavesRow(t *testing.T) {
	cases := []struct {
		name     string
		backend  string
		path     string
		attempts int32
	}{
		{"retire", "lost-retire", pathOK, 0},
		{"retry", "lost-retry", pathFail, 0},
		{"dlq move", "lost-dlq", pathFail, core.MaxCleanupAttempts - 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, s := newFakeDeleter(beA, tc.backend), newFakeStore()
			s.lost[2] = true
			before, beforeDLQ := statusCounts(), dlqEnqueued(tc.backend)
			sum := settleWith(d, s, row(1, beA, pathOK, 0), row(2, tc.backend, tc.path, tc.attempts))

			if sum.Succeeded != 1 || sum.Skipped != 1 || sum.Failed != 0 {
				t.Errorf("summary = %+v, want 1 succeeded and 1 skipped", sum)
			}
			if got := s.sortedRetired(); !slices.Equal(got, []int64{1}) {
				t.Errorf("retired %v, want row 1 only", got)
			}
			assertStatusDeltas(t, before, map[string]float64{statusSuccess: 1, statusClaimLost: 1})
			if got := dlqEnqueued(tc.backend) - beforeDLQ; got != 0 {
				t.Errorf("dlq enqueued delta = %v, want 0", got)
			}
		})
	}
}

// TestSettle_StoreWriteFailures verifies a failed store write leaves its row
// uncounted: a failed retire leaves the rows skipped, and a failed retry or
// DLQ move reports the row failed without its metrics.
func TestSettle_StoreWriteFailures(t *testing.T) {
	cases := []struct {
		name     string
		backend  string
		path     string
		attempts int32
		fail     func(*fakeStore)
		want     batch.Summary
	}{
		{"retire", "fail-retire", pathOK, 0, func(s *fakeStore) { s.completeErr = errStore }, batch.Summary{Skipped: 1}},
		{"retry", "fail-retry", pathFail, 0, func(s *fakeStore) { s.retryErr = errStore }, batch.Summary{Failed: 1}},
		{"dlq move", "fail-dlq", pathFail, core.MaxCleanupAttempts - 1, func(s *fakeStore) { s.dlqErr = errStore }, batch.Summary{Failed: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, s := newFakeDeleter(tc.backend), newFakeStore()
			tc.fail(s)
			before, beforeDLQ := statusCounts(), dlqEnqueued(tc.backend)
			sum := settleWith(d, s, row(1, tc.backend, tc.path, tc.attempts))

			if sum.Succeeded != tc.want.Succeeded || sum.Failed != tc.want.Failed || sum.Skipped != tc.want.Skipped {
				t.Errorf("summary = %+v, want %+v", sum, tc.want)
			}
			if s.writes() != 1 {
				t.Errorf("store writes = %d, want the one attempt", s.writes())
			}
			assertStatusDeltas(t, before, nil)
			if got := dlqEnqueued(tc.backend) - beforeDLQ; got != 0 {
				t.Errorf("dlq enqueued delta = %v, want 0", got)
			}
		})
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
	if got := s.sortedRetired(); !slices.Equal(got, []int64{1, 2, 3}) {
		t.Errorf("retired %v, want every row", got)
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
		row(1, beA, "k!1", 0), row(2, beA, pathFail, 0),
	})
	if len(d.calls) != 0 {
		t.Errorf("delete calls %v, want none under a declined admission", d.calls)
	}
	if s.writes() != 0 {
		t.Error("a row whose admission was declined was given an outcome")
	}
	if sum.Skipped != 2 || sum.Attempted != 0 {
		t.Errorf("summary = %+v, want both rows skipped", sum)
	}
}

// TestSettle_LogsCycleOnlyWhenNamed verifies the cycle log line is emitted
// under the settler's name and omitted when it has none.
func TestSettle_LogsCycleOnlyWhenNamed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		want bool
	}{
		{"", false},
		{"settle-log", true},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		st := New(Deps{Deleter: newFakeDeleter(beA), Store: newFakeStore(), Name: tc.name})
		st.log = slog.New(slog.NewTextHandler(&buf, nil))
		st.Settle(context.Background(), []core.CleanupItem{row(1, beA, pathOK, 0)})

		if got := strings.Contains(buf.String(), tc.name+" cycle complete"); got != tc.want {
			t.Errorf("name %q: logged cycle = %v, want %v (log %q)", tc.name, got, tc.want, buf.String())
		}
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
	d.onDelete = cancel

	New(Deps{Deleter: d, Store: s}).Settle(ctx, []core.CleanupItem{row(1, beA, pathOK, 0), row(2, beA, pathFail, 0)})

	if len(s.retired) != 1 || len(s.retried) != 1 {
		t.Fatalf("retired %v, retried %v; want both outcomes written", s.retired, s.retried)
	}
	for _, err := range s.ctxErrs {
		if err != nil {
			t.Errorf("an outcome was written on a cancelled context: %v", err)
		}
	}
}
