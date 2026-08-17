// -------------------------------------------------------------------------------
// Worker Cycle Outcome Tests
//
// Author: Alex Freidah
//
// Every worker reports how its cycle ended on a runs-total counter, taken from
// the per-item tally rather than from the fact that the cycle returned. These
// tests pin that label for each outcome a cycle can reach, because the failure
// they guard against is silent: a cycle that reports "success" regardless looks
// identical on a dashboard to one that actually succeeded, and a test that only
// asserts return values would never notice.
//
// Counters are process-wide and shared with the rest of the package, so every
// test here reads a delta around the call and none of them run in parallel.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// outcomeLabels is every label a cycle can report itself under. Asserting on
// all of them, rather than only the expected one, is what stops a cycle passing
// by reporting two.
var outcomeLabels = []string{OutcomeSuccess, OutcomePartial, OutcomeFailed, OutcomeEmpty, OutcomeError}

// wantOutcomeLabel asserts fn moved exactly one of the counters counterFor
// resolves - the one named by want - and left the rest alone. Counters are
// process-wide, so every assertion here is a delta around the call.
func wantOutcomeLabel(t *testing.T, counterFor func(string) prometheus.Counter, want string, fn func()) {
	t.Helper()
	before := make(map[string]float64, len(outcomeLabels))
	for _, l := range outcomeLabels {
		before[l] = readCounterValue(t, counterFor(l))
	}
	fn()
	for _, l := range outcomeLabels {
		got := readCounterValue(t, counterFor(l)) - before[l]
		wantDelta := float64(0)
		if l == want {
			wantDelta = 1
		}
		if got != wantDelta {
			t.Errorf("runs_total{outcome=%q} moved by %v, want %v", l, got, wantDelta)
		}
	}
}

// wantCycleLabel is wantOutcomeLabel for the workers whose runs-total carries
// the outcome as its only label.
func wantCycleLabel(t *testing.T, vec *prometheus.CounterVec, want string, fn func()) {
	t.Helper()
	wantOutcomeLabel(t, func(l string) prometheus.Counter { return vec.WithLabelValues(l) }, want, fn)
}

// -------------------------------------------------------------------------
// OUTCOME VOCABULARY
// -------------------------------------------------------------------------

// TestWorkSummary_OutcomeVocabulary pins the label each tally reports under.
// These strings are the contract operator alert rules are written against.
func TestWorkSummary_OutcomeVocabulary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		sum  WorkSummary
		want string
	}{
		{"all succeeded", WorkSummary{Succeeded: 3}, OutcomeSuccess},
		{"some failed", WorkSummary{Succeeded: 2, Failed: 1}, OutcomePartial},
		{"all failed", WorkSummary{Failed: 3}, OutcomeFailed},
		{"nothing to do", WorkSummary{}, OutcomeEmpty},
		{"only skipped", WorkSummary{Skipped: 4}, OutcomeEmpty},
		{"deferred only", WorkSummary{Deferred: 5}, OutcomeEmpty},
	}
	for _, tc := range cases {
		if got := tc.sum.Outcome(); got != tc.want {
			t.Errorf("%s: Outcome() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// -------------------------------------------------------------------------
// REPLICATION CYCLES
// -------------------------------------------------------------------------

// replicaFleet is the collaborator set a replication cycle runs against: two
// backends, admission always granted, and a placement that hands out b2 until
// the caller excludes it.
type replicaFleet struct {
	ops   *MockOps
	pl    *MockPlacement
	store *mockMetadataStore
}

// newReplicaFleet wires the mocks a full Replicate cycle needs. Targets are
// handed out in the order given, skipping any the replicator has excluded, and
// selection fails once they are all spent - which is what a real fleet does
// when every backend already holds a copy.
func newReplicaFleet(t *testing.T, store *mockMetadataStore, source string, targets ...string) *replicaFleet {
	t.Helper()
	ctrl := gomock.NewController(t)
	f := &replicaFleet{ops: NewMockOps(ctrl), pl: NewMockPlacement(ctrl), store: store}

	fleet := map[string]backend.ObjectBackend{source: backendtest.NewMockObjectBackend(ctrl)}
	for _, name := range targets {
		fleet[name] = backendtest.NewMockObjectBackend(ctrl)
	}
	f.ops.EXPECT().Backends().Return(fleet).AnyTimes()
	f.ops.EXPECT().GetBackend(gomock.Any()).DoAndReturn(func(name string) (backend.ObjectBackend, error) {
		be, ok := fleet[name]
		if !ok {
			return nil, errors.New("no such backend: " + name)
		}
		return be, nil
	}).AnyTimes()
	f.ops.EXPECT().AcquireAdmission(gomock.Any()).Return(true).AnyTimes()
	f.ops.EXPECT().ReleaseAdmission().AnyTimes()
	f.ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()

	f.pl.EXPECT().SelectReplicaTarget(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ int64, exclude map[string]bool) (string, error) {
			for _, name := range targets {
				if !exclude[name] {
					return name, nil
				}
			}
			return "", errors.New("no backend with space")
		}).AnyTimes()
	return f
}

// replicator builds the worker under test from the fleet.
func (f *replicaFleet) replicator() *Replicator {
	return NewReplicator(f.ops, f.pl, f.store)
}

// copyFails makes the streaming copy of key fail, standing in for a source
// backend that dies partway through the transfer.
func (f *replicaFleet) copyFails(key string) {
	f.ops.EXPECT().StreamCopy(gomock.Any(), gomock.Any(), gomock.Any(), key).
		Return(errors.New("stream copy: connection reset")).AnyTimes()
}

// copySucceeds lets every remaining copy through.
func (f *replicaFleet) copySucceeds() {
	f.ops.EXPECT().StreamCopy(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil).AnyTimes()
}

// copyOn returns one existing copy row for key, the shape
// GetUnderReplicatedObjects hands the replicator.
func copyOn(key, backendName string) core.ObjectLocation {
	return core.ObjectLocation{ObjectKey: key, BackendName: backendName, SizeBytes: 100}
}

// replicationConfig is a two-copy cycle sized for a single-object batch.
func replicationConfig(factor int) config.ReplicationConfig {
	return config.ReplicationConfig{Factor: factor, BatchSize: 10, Concurrency: 1}
}

// TestReplicate_ReportsCycleOutcome asserts a replication cycle labels its
// runs-total by what the objects actually did. Before the tally drove the
// label, every cycle that returned without a query error reported success -
// including the two below where objects were left under-replicated.
func TestReplicate_ReportsCycleOutcome(t *testing.T) {
	cases := []struct {
		name string
		keys []string
		// failing lists the keys whose copy dies mid-transfer.
		failing []string
		want    string
	}{
		{name: "every object copied", keys: []string{"key1"}, want: OutcomeSuccess},
		{name: "every object failed", keys: []string{"key1"}, failing: []string{"key1"}, want: OutcomeFailed},
		{
			name:    "one of two objects failed",
			keys:    []string{"key1", "key2"},
			failing: []string{"key2"},
			want:    OutcomePartial,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows := make([]core.ObjectLocation, 0, len(tc.keys))
			for _, key := range tc.keys {
				rows = append(rows, copyOn(key, "b1"))
			}
			store := &mockMetadataStore{underReplicated: rows, recordReplicaOK: true, recordReplicaSize: 100}
			f := newReplicaFleet(t, store, "b1", "b2")
			for _, key := range tc.failing {
				f.copyFails(key)
			}
			f.copySucceeds()

			wantCycleLabel(t, telemetry.ReplicationRunsTotal, tc.want, func() {
				if _, err := f.replicator().Replicate(context.Background(), replicationConfig(2), nil); err != nil {
					t.Fatalf("Replicate: %v", err)
				}
			})
		})
	}
}

// TestReplicate_EmptyCycleIsNotSuccess asserts a cycle with nothing to do says
// so, rather than claiming it replicated something.
func TestReplicate_EmptyCycleIsNotSuccess(t *testing.T) {
	f := newReplicaFleet(t, &mockMetadataStore{}, "b1", "b2")

	wantCycleLabel(t, telemetry.ReplicationRunsTotal, OutcomeEmpty, func() {
		sum, err := f.replicator().Replicate(context.Background(), replicationConfig(2), nil)
		if err != nil {
			t.Fatalf("Replicate: %v", err)
		}
		if sum.CopiesCreated != 0 {
			t.Errorf("CopiesCreated = %d, want 0", sum.CopiesCreated)
		}
	})
}

// TestReplicate_QueryFailureReportsError asserts a cycle that never got a batch
// to work on is labelled error, not empty: it has no tally to classify.
func TestReplicate_QueryFailureReportsError(t *testing.T) {
	f := newReplicaFleet(t, &mockMetadataStore{underReplicatedErr: errors.New("ledger unavailable")}, "b1", "b2")

	wantCycleLabel(t, telemetry.ReplicationRunsTotal, OutcomeError, func() {
		if _, err := f.replicator().Replicate(context.Background(), replicationConfig(2), nil); err == nil {
			t.Fatal("Replicate returned nil error despite the query failing")
		}
	})
}

// TestReplicate_AdmissionBlockedCycleIsNotSuccess asserts a cycle the admission
// gate turned away reports the work it declined rather than a clean pass.
func TestReplicate_AdmissionBlockedCycleIsNotSuccess(t *testing.T) {
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{
		"b1": backendtest.NewMockObjectBackend(ctrl),
	}).AnyTimes()
	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(false).AnyTimes()
	store := &mockMetadataStore{underReplicated: []core.ObjectLocation{copyOn("key1", "b1")}}

	r := NewReplicator(ops, NewMockPlacement(ctrl), store)
	wantCycleLabel(t, telemetry.ReplicationRunsTotal, OutcomeEmpty, func() {
		sum, err := r.Replicate(context.Background(), replicationConfig(2), nil)
		if err != nil {
			t.Fatalf("Replicate: %v", err)
		}
		if sum.Skipped != 1 {
			t.Errorf("Skipped = %d, want the one object admission turned away", sum.Skipped)
		}
	})
}

// TestReplicate_SummaryCountsObjectsAndCopies asserts the two counts a cycle
// reports mean different things. One object needing two copies is one
// successful item and two copies created; conflating them would report the
// fleet did half the work it did.
func TestReplicate_SummaryCountsObjectsAndCopies(t *testing.T) {
	store := &mockMetadataStore{
		underReplicated:   []core.ObjectLocation{copyOn("key1", "b1")},
		recordReplicaOK:   true,
		recordReplicaSize: 100,
	}
	f := newReplicaFleet(t, store, "b1", "b2", "b3")
	f.copySucceeds()

	sum, err := f.replicator().Replicate(context.Background(), replicationConfig(3), nil)
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if sum.CopiesCreated != 2 {
		t.Errorf("CopiesCreated = %d, want 2", sum.CopiesCreated)
	}
	if sum.Planned != 1 || sum.Succeeded != 1 || sum.Failed != 0 {
		t.Errorf("tally = %+v, want one planned object that succeeded", sum.WorkSummary)
	}
}

// TestReplicaOutcomeResult_ClassifiesAnObject pins how one object's copy
// attempts fold into the batch tally, including the rule that any failed
// attempt fails the item even when another copy of the same object landed:
// the object did not reach its factor, so the cycle is not clean.
func TestReplicaOutcomeResult_ClassifiesAnObject(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		out  ReplicationOutcome
		want ItemOutcome
	}{
		{"copies made", ReplicationOutcome{Created: 2}, ItemSucceeded},
		{"copy errored", ReplicationOutcome{CopyErrors: 1}, ItemFailed},
		{"record errored", ReplicationOutcome{RecordErrors: 1}, ItemFailed},
		{"source superseded", ReplicationOutcome{Superseded: 1}, ItemFailed},
		{"partly created still fails", ReplicationOutcome{Created: 1, CopyErrors: 1}, ItemFailed},
		{"nowhere to put it", ReplicationOutcome{NoTarget: true}, ItemSkipped},
	}
	for _, tc := range cases {
		if got := replicaOutcomeResult(&tc.out); got.Outcome != tc.want {
			t.Errorf("%s: outcome = %v, want %v", tc.name, got.Outcome, tc.want)
		}
	}
}

// -------------------------------------------------------------------------
// OVER-REPLICATION CYCLES
// -------------------------------------------------------------------------

// cleanerFleet is the collaborator set an over-replication cycle runs against.
// Any backend name outside the fleet map - "gone" below - scores lowest, so a
// copy stranded there is picked as the victim and then fails to resolve.
type cleanerFleet struct {
	ops   *MockOps
	pl    *MockPlacement
	store *mockMetadataStore
}

// newCleanerFleet wires the mocks a full Clean cycle needs.
func newCleanerFleet(t *testing.T, store *mockMetadataStore) *cleanerFleet {
	t.Helper()
	ctrl := gomock.NewController(t)
	f := &cleanerFleet{ops: NewMockOps(ctrl), pl: NewMockPlacement(ctrl), store: store}

	fleet := map[string]backend.ObjectBackend{
		"b1": backendtest.NewMockObjectBackend(ctrl),
		"b2": backendtest.NewMockObjectBackend(ctrl),
		"b3": backendtest.NewMockObjectBackend(ctrl),
		"b4": backendtest.NewMockObjectBackend(ctrl),
	}
	f.ops.EXPECT().Backends().Return(fleet).AnyTimes()
	f.ops.EXPECT().IsDraining(gomock.Any()).Return(false).AnyTimes()
	f.ops.EXPECT().GetBackend(gomock.Any()).DoAndReturn(func(name string) (backend.ObjectBackend, error) {
		be, ok := fleet[name]
		if !ok {
			return nil, errors.New("no such backend: " + name)
		}
		return be, nil
	}).AnyTimes()
	f.ops.EXPECT().AcquireAdmission(gomock.Any()).Return(true).AnyTimes()
	f.ops.EXPECT().ReleaseAdmission().AnyTimes()
	f.pl.EXPECT().DeleteOrEnqueue(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
	return f
}

// cleaner builds the worker under test from the fleet.
func (f *cleanerFleet) cleaner() *OverReplicationCleaner {
	return NewOverReplicationCleaner(f.ops, f.pl, f.store)
}

// overReplicatedOn returns one key's copies, one row per backend.
func overReplicatedOn(key string, backends ...string) []core.ObjectLocation {
	rows := make([]core.ObjectLocation, 0, len(backends))
	for _, name := range backends {
		rows = append(rows, core.ObjectLocation{ObjectKey: key, BackendName: name, SizeBytes: 100})
	}
	return rows
}

// TestClean_ReportsCycleOutcome asserts a cleanup cycle labels its runs-total
// by what the objects actually did. keyB's surplus copy sits on a backend that
// has left the fleet, so its removal cannot complete.
func TestClean_ReportsCycleOutcome(t *testing.T) {
	cleanable := overReplicatedOn("keyA", "b1", "b2", "b3")
	stranded := overReplicatedOn("keyB", "gone", "b2", "b3")

	cases := []struct {
		name string
		rows []core.ObjectLocation
		want string
	}{
		{name: "surplus removed", rows: cleanable, want: OutcomeSuccess},
		{name: "removal could not complete", rows: stranded, want: OutcomeFailed},
		{name: "one object of two failed", rows: append(append([]core.ObjectLocation{}, cleanable...), stranded...), want: OutcomePartial},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCleanerFleet(t, &mockMetadataStore{overReplicated: tc.rows})
			wantCycleLabel(t, telemetry.OverReplicationRunsTotal, tc.want, func() {
				if _, err := f.cleaner().Clean(context.Background(), replicationConfig(2), nil); err != nil {
					t.Fatalf("Clean: %v", err)
				}
			})
		})
	}
}

// TestClean_MetadataRefusalIsAFailure asserts a removal the store refused
// outright is reported as a failed item. The cycle left the fleet over its
// factor, which is a state an operator has to be told about.
func TestClean_MetadataRefusalIsAFailure(t *testing.T) {
	f := newCleanerFleet(t, &mockMetadataStore{
		overReplicated:  overReplicatedOn("keyA", "b1", "b2", "b3"),
		removeExcessErr: errors.New("deadlock detected"),
	})

	wantCycleLabel(t, telemetry.OverReplicationRunsTotal, OutcomeFailed, func() {
		sum, err := f.cleaner().Clean(context.Background(), replicationConfig(2), nil)
		if err != nil {
			t.Fatalf("Clean: %v", err)
		}
		if sum.CopiesRemoved != 0 {
			t.Errorf("CopiesRemoved = %d, want 0", sum.CopiesRemoved)
		}
	})
}

// TestClean_BenignRaceIsNotAFailure asserts an object whose excess was already
// absorbed elsewhere is skipped rather than failed. Reporting it as failed
// would page an operator every time a client delete raced a cleanup tick.
func TestClean_BenignRaceIsNotAFailure(t *testing.T) {
	f := newCleanerFleet(t, &mockMetadataStore{
		overReplicated:   overReplicatedOn("keyA", "b1", "b2", "b3"),
		removeExcessNoOp: true,
	})

	wantCycleLabel(t, telemetry.OverReplicationRunsTotal, OutcomeEmpty, func() {
		sum, err := f.cleaner().Clean(context.Background(), replicationConfig(2), nil)
		if err != nil {
			t.Fatalf("Clean: %v", err)
		}
		if sum.Skipped != 1 || sum.Failed != 0 {
			t.Errorf("tally = %+v, want the raced object skipped and nothing failed", sum.WorkSummary)
		}
	})
}

// TestClean_QueryFailureReportsError asserts a cycle that never got a batch is
// labelled error rather than empty.
func TestClean_QueryFailureReportsError(t *testing.T) {
	f := newCleanerFleet(t, &mockMetadataStore{overReplicatedErr: errors.New("ledger unavailable")})

	wantCycleLabel(t, telemetry.OverReplicationRunsTotal, OutcomeError, func() {
		if _, err := f.cleaner().Clean(context.Background(), replicationConfig(2), nil); err == nil {
			t.Fatal("Clean returned nil error despite the query failing")
		}
	})
}

// TestClean_SummaryCountsObjectsAndCopies asserts the two counts a cycle
// reports mean different things: one object carrying two surplus copies is one
// successful item and two copies removed.
func TestClean_SummaryCountsObjectsAndCopies(t *testing.T) {
	f := newCleanerFleet(t, &mockMetadataStore{overReplicated: overReplicatedOn("keyA", "b1", "b2", "b3", "b4")})

	sum, err := f.cleaner().Clean(context.Background(), replicationConfig(2), nil)
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if sum.CopiesRemoved != 2 {
		t.Errorf("CopiesRemoved = %d, want 2", sum.CopiesRemoved)
	}
	if sum.Planned != 1 || sum.Succeeded != 1 || sum.Failed != 0 {
		t.Errorf("tally = %+v, want one planned object that succeeded", sum.WorkSummary)
	}
}

// TestCleanupItemResult_ClassifiesAnObject pins how one object's surplus
// removals fold into the batch tally: anything removed is progress, an object
// that gave up nothing without erroring was a benign race.
func TestCleanupItemResult_ClassifiesAnObject(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		removed  int
		failures int
		want     ItemOutcome
	}{
		{"surplus removed", 2, 0, ItemSucceeded},
		{"removed some, failed some", 1, 1, ItemSucceeded},
		{"every removal failed", 0, 2, ItemFailed},
		{"nothing left to remove", 0, 0, ItemSkipped},
	}
	for _, tc := range cases {
		if got := cleanupItemResult(tc.removed, tc.failures); got.Outcome != tc.want {
			t.Errorf("%s: outcome = %v, want %v", tc.name, got.Outcome, tc.want)
		}
	}
}

// -------------------------------------------------------------------------
// REBALANCE CYCLES
// -------------------------------------------------------------------------

// spreadConfig is an imbalanced-fleet cycle that always clears the threshold.
var spreadConfig = config.RebalanceConfig{Strategy: "spread", BatchSize: 10, Concurrency: 1, Threshold: 0.1}

// wantRebalanceLabel is wantOutcomeLabel for the rebalancer, whose runs-total
// carries the strategy alongside the outcome.
func wantRebalanceLabel(t *testing.T, want string, fn func()) {
	t.Helper()
	wantOutcomeLabel(t, func(l string) prometheus.Counter {
		return telemetry.RebalanceRunsTotal.WithLabelValues(spreadConfig.Strategy, l)
	}, want, fn)
}

// newRebalanceFleet wires a two-backend fleet skewed enough to plan one move,
// whose single move returns moveErr.
func newRebalanceFleet(t *testing.T, moveErr error) *Rebalancer {
	t.Helper()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	store := &mockMetadataStore{
		quotaStats: map[string]core.QuotaStat{
			"b1": {BytesUsed: 800, BytesLimit: 1000},
			"b2": {BytesUsed: 200, BytesLimit: 1000},
		},
		objectsByBackend: map[string][]core.ObjectLocation{
			"b1": {{ObjectKey: "key1", BackendName: "b1", SizeBytes: 100}},
		},
	}
	ops.EXPECT().BackendOrder().Return([]string{"b1", "b2"}).AnyTimes()
	ops.EXPECT().Usage().Return(unlimitedUsage()).AnyTimes()
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{
		"b1": backendtest.NewMockObjectBackend(ctrl),
		"b2": backendtest.NewMockObjectBackend(ctrl),
	}).AnyTimes()
	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(true).AnyTimes()
	ops.EXPECT().ReleaseAdmission().AnyTimes()
	pl.EXPECT().MoveObject(gomock.Any(), gomock.Any()).Return(int64(100), moveErr).AnyTimes()
	return NewRebalancer(ops, pl, store)
}

// TestRebalance_ReportsCycleOutcome asserts a rebalance cycle labels its
// runs-total by whether the planned moves landed. A cycle whose every move
// failed used to report the same label as one that moved everything.
func TestRebalance_ReportsCycleOutcome(t *testing.T) {
	cases := []struct {
		name    string
		moveErr error
		want    string
	}{
		{name: "moves landed", want: OutcomeSuccess},
		{name: "every move failed", moveErr: errors.New("stream copy: timeout"), want: OutcomeFailed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRebalanceFleet(t, tc.moveErr)
			wantRebalanceLabel(t, tc.want, func() {
				if _, err := r.Rebalance(context.Background(), spreadConfig, nil); err != nil {
					t.Fatalf("Rebalance: %v", err)
				}
			})
		})
	}
}

// TestRebalance_QuotaQueryFailureReportsError asserts a cycle that could not
// read the fleet's utilization is labelled error rather than skipped.
func TestRebalance_QuotaQueryFailureReportsError(t *testing.T) {
	ctrl := gomock.NewController(t)
	r := NewRebalancer(NewMockOps(ctrl), NewMockPlacement(ctrl),
		&mockMetadataStore{quotaStatsErr: errors.New("ledger unavailable")})

	wantRebalanceLabel(t, OutcomeError, func() {
		if _, err := r.Rebalance(context.Background(), spreadConfig, nil); err == nil {
			t.Fatal("Rebalance returned nil error despite the quota query failing")
		}
	})
}
