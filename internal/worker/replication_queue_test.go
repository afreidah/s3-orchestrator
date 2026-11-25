// -------------------------------------------------------------------------------
// Replication Queue Tests
//
// Author: Alex Freidah
//
// Pins how the workers drain the replication queue: a read hands out the keys
// off the factor with their copies and settles the ones that need nothing, a
// pass settles or retries each key it worked, a failed release ends the pass,
// and the queue rebuilds only when its basis changed or an operator asks.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
)

// shortOf counts how many copies a queued key is short of factor, as the
// replicator does.
func shortOf(factor int) func(*core.QueuedReplication) int {
	return func(q *core.QueuedReplication) int { return factor - q.Live }
}

// offByKey maps each copy set's key to how far off the factor it is.
func offByKey(sets []copySet) map[string]int {
	got := make(map[string]int, len(sets))
	for _, s := range sets {
		got[s.key] = s.off
	}
	return got
}

// TestReplicationQueue_ReadHandsOutKeysOffTheFactor verifies a read hands out
// each queued key short of the factor with how many copies it needs, settles
// a key read back at or past the factor, and reports every key it read.
func TestReplicationQueue_ReadHandsOutKeysOffTheFactor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		locations   []core.ObjectLocation
		factor      int
		wantSets    map[string]int
		wantSettled []string
	}{
		{name: "empty queue reads nothing", factor: 3, wantSets: map[string]int{}},
		{
			name:      "single copy at factor 2 needs one more",
			locations: []core.ObjectLocation{{ObjectKey: "k1", BackendName: "b1"}},
			factor:    2,
			wantSets:  map[string]int{"k1": 1},
		},
		{
			name: "mixed keys keep only the short ones",
			locations: []core.ObjectLocation{
				{ObjectKey: "needs2", BackendName: "b1"},
				{ObjectKey: "needs1", BackendName: "b1"},
				{ObjectKey: "needs1", BackendName: "b2"},
				{ObjectKey: "satisfied", BackendName: "b1"},
				{ObjectKey: "satisfied", BackendName: "b2"},
				{ObjectKey: "satisfied", BackendName: "b3"},
			},
			factor:      3,
			wantSets:    map[string]int{"needs2": 2, "needs1": 1},
			wantSettled: []string{"satisfied"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ms := &mockMetadataStore{underReplicated: tc.locations}
			sets, err := NewReplicationQueue(ms).read(t.Context(), core.ReplicationShort, 100, nil, shortOf(tc.factor))
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if got := offByKey(sets); !maps.Equal(got, tc.wantSets) {
				t.Errorf("sets = %v, want %v", got, tc.wantSets)
			}
			if !slices.Equal(ms.settledKeys, tc.wantSettled) {
				t.Errorf("settled = %v, want %v", ms.settledKeys, tc.wantSettled)
			}
		})
	}
}

// TestReplicationQueue_ReadOverSideCountsSurplus verifies the cleaner's side:
// a key past the factor carries its surplus, and one read back at the factor
// is settled.
func TestReplicationQueue_ReadOverSideCountsSurplus(t *testing.T) {
	t.Parallel()
	ms := &mockMetadataStore{overReplicated: []core.ObjectLocation{
		{ObjectKey: "over2", BackendName: "b1"},
		{ObjectKey: "over2", BackendName: "b2"},
		{ObjectKey: "over2", BackendName: "b3"},
		{ObjectKey: "at", BackendName: "b1"},
	}}
	sets, err := NewReplicationQueue(ms).read(t.Context(), core.ReplicationOver, 100, nil, func(q *core.QueuedReplication) int {
		return q.Held - 1
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := offByKey(sets); !maps.Equal(got, map[string]int{"over2": 2}) {
		t.Errorf("sets = %v, want over2 by 2", got)
	}
	if !slices.Equal(ms.settledKeys, []string{"at"}) {
		t.Errorf("settled = %v, want [at]", ms.settledKeys)
	}
}

// TestReplicationQueue_ReadHandsOutEveryCopy verifies a copy set carries every
// copy the ledger records for its key, which the replicator excludes as
// targets.
func TestReplicationQueue_ReadHandsOutEveryCopy(t *testing.T) {
	t.Parallel()
	ms := &mockMetadataStore{underReplicated: []core.ObjectLocation{
		{ObjectKey: "k1", BackendName: "b1", SizeBytes: 100},
		{ObjectKey: "k1", BackendName: "b2", SizeBytes: 100},
	}}
	sets, err := NewReplicationQueue(ms).read(t.Context(), core.ReplicationShort, 100, nil, shortOf(3))
	if err != nil || len(sets) != 1 {
		t.Fatalf("read = %+v, %v; want one set", sets, err)
	}
	backends := make([]string, 0, len(sets[0].copies))
	for _, c := range sets[0].copies {
		backends = append(backends, c.BackendName)
	}
	slices.Sort(backends)
	if !slices.Equal(backends, []string{"b1", "b2"}) {
		t.Errorf("copies on %v, want b1 and b2", backends)
	}
}

// TestReplicationQueue_ReadReleasesKeysItCannotWork verifies a key whose copies
// are gone is settled, and one whose copies could not be read, or that has no
// copy recorded yet but a live intent, is held for a retry, none handed out.
func TestReplicationQueue_ReadReleasesKeysItCannotWork(t *testing.T) {
	t.Parallel()
	queued := []core.ObjectLocation{{ObjectKey: "k1", BackendName: "b1"}}
	for _, tc := range []struct {
		name        string
		store       *mockMetadataStore
		wantSettled []string
		wantRetried []string
	}{
		{"gone", &mockMetadataStore{underReplicated: queued, allLocations: []core.ObjectLocation{}}, []string{"k1"}, nil},
		{"unreadable", &mockMetadataStore{underReplicated: queued, allLocationsErr: errors.New("db down")}, nil, []string{"k1"}},
		{"intent not yet promoted", &mockMetadataStore{shortIntentsOnly: []string{"k1"}, allLocations: []core.ObjectLocation{}}, nil, []string{"k1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sets, err := NewReplicationQueue(tc.store).read(t.Context(), core.ReplicationShort, 100, nil, shortOf(2))
			if err != nil || len(sets) != 0 {
				t.Fatalf("read = %+v, %v; want no sets", sets, err)
			}
			if !slices.Equal(tc.store.settledKeys, tc.wantSettled) || !slices.Equal(tc.store.retriedKeys, tc.wantRetried) {
				t.Errorf("settled %v retried %v, want %v and %v", tc.store.settledKeys, tc.store.retriedKeys, tc.wantSettled, tc.wantRetried)
			}
		})
	}
}

// TestReplicationQueue_FailedReleaseEndsTheRead verifies a read whose settle
// fails reports it, which fails the pass and leaves the key due.
func TestReplicationQueue_FailedReleaseEndsTheRead(t *testing.T) {
	t.Parallel()
	ms := &mockMetadataStore{
		underReplicated: []core.ObjectLocation{{ObjectKey: "k1", BackendName: "b1"}, {ObjectKey: "k1", BackendName: "b2"}},
		releaseErr:      errors.New("db down"),
	}
	if _, err := NewReplicationQueue(ms).read(t.Context(), core.ReplicationShort, 100, nil, shortOf(2)); err == nil {
		t.Fatal("read: want the failed settle")
	}
}

// TestReplicationQueue_PrepareRebuildsOnlyWhenItsBasisChanged verifies the
// queue rebuilds on its first pass and after the excluded backends or the
// fleet changed, and on every pass at a factor the store's writes are not
// queueing against.
func TestReplicationQueue_PrepareRebuildsOnlyWhenItsBasisChanged(t *testing.T) {
	t.Parallel()
	ms := &mockMetadataStore{replicationFactor: 2}
	q := NewReplicationQueue(ms)
	two := map[string]backend.ObjectBackend{"b1": nil, "b2": nil}
	three := map[string]backend.ObjectBackend{"b1": nil, "b2": nil, "b3": nil}
	steps := []struct {
		name     string
		factor   int
		excluded []string
		backends map[string]backend.ObjectBackend
		rebuilds int
	}{
		{"first pass", 2, nil, two, 1},
		{"same basis", 2, nil, two, 1},
		{"excluded changed", 2, []string{"b2"}, two, 2},
		{"fleet changed", 2, []string{"b2"}, three, 3},
		{"factor the writes do not keep", 3, []string{"b2"}, three, 4},
		{"same ad hoc factor", 3, []string{"b2"}, three, 5},
	}
	for _, s := range steps {
		if err := q.prepare(t.Context(), s.factor, s.excluded, s.backends); err != nil {
			t.Fatalf("%s: prepare: %v", s.name, err)
		}
		if ms.rebuilds != s.rebuilds {
			t.Errorf("%s: rebuilds = %d, want %d", s.name, ms.rebuilds, s.rebuilds)
		}
	}
}

// TestReplicator_RebuildQueueAlwaysRebuilds verifies an operator's rebuild runs
// even when nothing the queue was last rebuilt against has changed, and reports
// what it queued.
func TestReplicator_RebuildQueueAlwaysRebuilds(t *testing.T) {
	t.Parallel()
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().RebuildReplicationQueue(gomock.Any(), 2, gomock.Any()).Return(int64(3), nil).Times(2)
	storetest.Permissive(store)
	w := newReplicatorFor(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, &fleetOpts{})
	for range 2 {
		if queued, err := w.RebuildQueue(t.Context(), config.ReplicationConfig{Factor: 2}); err != nil || queued != 3 {
			t.Fatalf("RebuildQueue = %d, %v; want 3 queued", queued, err)
		}
	}
}

// TestCopySetPass_SettlesDoneAndRetriesTheRest verifies a pass settles the key
// whose set reached the factor, retries the one that did not, and sums the
// copies changed.
func TestCopySetPass_SettlesDoneAndRetriesTheRest(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := newMockOps(ctrl)
	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(func() {}, true).AnyTimes()
	ms := &mockMetadataStore{}
	sets := []copySet{
		{key: "done", off: 1, queued: &core.QueuedReplication{Key: "done"}},
		{key: "short", off: 2, queued: &core.QueuedReplication{Key: "short"}},
	}

	pass := copySetPass{queue: NewReplicationQueue(ms), concurrency: 1}
	sum, changed, err := pass.run(t.Context(), ops, sets, func(_ context.Context, set *copySet) (int, bool, batch.ItemResult) {
		return 1, set.key == "done", batch.Succeeded()
	})
	if err != nil || sum.Planned != 2 || changed != 2 {
		t.Fatalf("run = %+v, %d changed, %v; want 2 planned and 2 changed", sum, changed, err)
	}
	if !slices.Equal(ms.settledKeys, []string{"done"}) || !slices.Equal(ms.retriedKeys, []string{"short"}) {
		t.Errorf("settled %v retried %v, want [done] and [short]", ms.settledKeys, ms.retriedKeys)
	}
}

// TestCopySetPass_FailedReleaseIsItsError verifies a pass whose settle fails
// reports it after working every set.
func TestCopySetPass_FailedReleaseIsItsError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := newMockOps(ctrl)
	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(func() {}, true).AnyTimes()
	ms := &mockMetadataStore{releaseErr: errors.New("db down")}
	sets := []copySet{{key: "k1", off: 1, queued: &core.QueuedReplication{Key: "k1"}}}

	pass := copySetPass{queue: NewReplicationQueue(ms), concurrency: 1}
	if _, _, err := pass.run(t.Context(), ops, sets, func(context.Context, *copySet) (int, bool, batch.ItemResult) {
		return 1, true, batch.Succeeded()
	}); !errors.Is(err, ms.releaseErr) {
		t.Errorf("run err = %v, want the failed settle", err)
	}
}
