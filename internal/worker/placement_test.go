// -------------------------------------------------------------------------------
// Rebalancer Planning Helper Tests - PlacementSet and candidateCache
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// TestPlacementSet_Has covers the membership check: present keys/backends
// report true, absent keys or backends report false (including a missing key).
func TestPlacementSet_Has(t *testing.T) {
	t.Parallel()
	ps := placementSet{}
	ps.addAll(map[string][]string{
		"k1": {"b1", "b2"},
		"k2": {"b3"},
	})
	cases := []struct {
		key, backend string
		want         bool
	}{
		{"k1", "b1", true},
		{"k1", "b2", true},
		{"k1", "b3", false},
		{"k2", "b3", true},
		{"k2", "b1", false},
		{"missing", "b1", false},
	}
	for _, c := range cases {
		if got := ps.Has(c.key, c.backend); got != c.want {
			t.Errorf("Has(%q, %q) = %v, want %v", c.key, c.backend, got, c.want)
		}
	}
}

// TestPlacementSet_AddAllKeepsRecordedCopies verifies a later copy map adds to
// the set, since a planned move is recorded only there: a fresh listing of the
// same key must not forget it.
func TestPlacementSet_AddAllKeepsRecordedCopies(t *testing.T) {
	t.Parallel()
	ps := placementSet{}
	ps.add("k", "planned")
	ps.addAll(map[string][]string{"k": {"b1"}})
	ps.addAll(nil)
	if !ps.Has("k", "planned") || !ps.Has("k", "b1") {
		t.Errorf("set = %v, want both copies", ps)
	}
}

// TestCandidateCache_MemoizesPerSource verifies the cache fetches a source's
// candidates once: a second forBackend for the same source returns the cached
// objects without re-querying the (expensive) backend copy map.
func TestCandidateCache_MemoizesPerSource(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := newMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{
		objectsByBackend:       map[string][]core.ObjectLocation{"b1": {{ObjectKey: "k1", BackendName: "b1"}}},
		getBackendsForKeysResp: map[string][]string{"k1": {"b1", "b2"}},
	}

	r := NewRebalancer(RebalancerDeps{Fleet: fleetOf(ops, pl), Store: ms, Config: defaultRebalance})
	holders := placementSet{}
	cc := r.newCandidateCache(10, holders)

	if _, err := cc.forBackend(context.Background(), "b1"); err != nil {
		t.Fatalf("forBackend (first): %v", err)
	}
	objects, err := cc.forBackend(context.Background(), "b1")
	if err != nil {
		t.Fatalf("forBackend (second): %v", err)
	}

	if ms.getBackendsForKeysCalls != 1 {
		t.Errorf("GetObjectBackendsForKeys called %d times, want 1 (second lookup must hit the cache)", ms.getBackendsForKeysCalls)
	}
	if !holders.Has("k1", "b2") {
		t.Error("holders missing k1 -> b2")
	}
	if len(objects) != 1 || objects[0].ObjectKey != "k1" {
		t.Errorf("cached objects = %+v, want one object k1", objects)
	}
}
