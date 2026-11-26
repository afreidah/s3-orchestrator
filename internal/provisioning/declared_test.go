// -------------------------------------------------------------------------------
// Provisioning - Declared Bucket Set Tests
//
// Author: Alex Freidah
//
// Covers what the live set reports before its first store, after one, and
// after a swap replaces it.
// -------------------------------------------------------------------------------

package provisioning

import (
	"slices"
	"testing"
)

// TestDeclared_ContainsFollowsTheStoredSet verifies Contains reports nothing
// before a store and from a nil holder, and the names of whichever set was
// stored last.
func TestDeclared_ContainsFollowsTheStoredSet(t *testing.T) {
	t.Parallel()
	var nilSet *Declared
	d := NewDeclared()
	if nilSet.Contains("photos") || d.Contains("photos") || d.Buckets() != nil {
		t.Fatal("an empty holder reported a bucket")
	}

	d.Store([]Bucket{{Name: "photos"}, {Name: "logs"}})
	if !d.Contains("photos") || !d.Contains("logs") || d.Contains("other") {
		t.Errorf("Contains does not match the stored set %v", d.Names())
	}
	if !d.HasPrefix("photos/a.jpg") || d.HasPrefix("photos") {
		t.Error("HasPrefix does not match the stored set")
	}

	d.Store([]Bucket{{Name: "other"}})
	if d.Contains("photos") || !d.Contains("other") || !slices.Equal(d.Names(), []string{"other"}) {
		t.Errorf("a swap did not replace the set: %v", d.Names())
	}
}
