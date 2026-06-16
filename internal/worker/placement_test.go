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

// TestPlacementSet_Has covers the O(1) membership check: present keys/backends
// report true, absent keys or backends report false (including a missing key).
func TestPlacementSet_Has(t *testing.T) {
	t.Parallel()
	ps := NewPlacementSet(map[string][]string{
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

// TestNewPlacementSet_Empty verifies a nil/empty copy map yields a set that
// reports no membership rather than panicking.
func TestNewPlacementSet_Empty(t *testing.T) {
	t.Parallel()
	ps := NewPlacementSet(nil)
	if ps.Has("k", "b") {
		t.Error("empty PlacementSet reported membership")
	}
}

// TestCandidateCache_MemoizesPerSource verifies the cache fetches a source's
// candidates once: a second forBackend for the same source returns the cached
// objects + placement without re-querying the (expensive) backend copy map.
func TestCandidateCache_MemoizesPerSource(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{
		objectsByBackend:       map[string][]core.ObjectLocation{"b1": {{ObjectKey: "k1", BackendName: "b1"}}},
		getBackendsForKeysResp: map[string][]string{"k1": {"b1", "b2"}},
	}

	r := NewRebalancer(ops, pl, ms)
	cc := r.newCandidateCache(10)

	sc1, err := cc.forBackend(context.Background(), "b1")
	if err != nil {
		t.Fatalf("forBackend (first): %v", err)
	}
	sc2, err := cc.forBackend(context.Background(), "b1")
	if err != nil {
		t.Fatalf("forBackend (second): %v", err)
	}

	if ms.getBackendsForKeysCalls != 1 {
		t.Errorf("GetObjectBackendsForKeys called %d times, want 1 (second lookup must hit the cache)", ms.getBackendsForKeysCalls)
	}
	if !sc1.Placement.Has("k1", "b2") {
		t.Error("placement set missing k1 -> b2")
	}
	if len(sc2.Objects) != 1 || sc2.Objects[0].ObjectKey != "k1" {
		t.Errorf("cached objects = %+v, want one object k1", sc2.Objects)
	}
}
