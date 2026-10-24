// -------------------------------------------------------------------------------
// Copy Lookup Helper
//
// Author: Alex Freidah
//
// Reads back the copy a store holds for a key on one backend, for the engine
// tests that hand a copy to a method keyed by the copy the caller read.
// -------------------------------------------------------------------------------

package storetest

import (
	"context"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// LocationLister is the one store read LocationOn needs, which both engines
// satisfy.
type LocationLister interface {
	GetAllObjectLocations(ctx context.Context, key string) ([]core.ObjectLocation, error)
}

// LocationOn returns the copy of key held on backendName, failing the test
// when the store holds none there.
func LocationOn(t testing.TB, s LocationLister, key, backendName string) *core.ObjectLocation {
	t.Helper()
	locs, err := s.GetAllObjectLocations(context.Background(), key)
	if err != nil {
		t.Fatalf("GetAllObjectLocations: %v", err)
	}
	loc := core.LocationOn(locs, backendName)
	if loc == nil {
		t.Fatalf("no copy of %q on %q; have %+v", key, backendName, locs)
	}
	return loc
}
