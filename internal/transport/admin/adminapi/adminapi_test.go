// -------------------------------------------------------------------------------
// Admin API - Path and Tag Conversion Tests
//
// Author: Alex Freidah
//
// Covers the path builder every client addresses a resource with and the tag
// conversions the handler and clients share.
// -------------------------------------------------------------------------------

package adminapi

import (
	"slices"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// TestPath_EscapesEachIdentifier verifies every identifier becomes its own
// escaped segment, a key's slashes included, and that no identifier leaves
// the base untouched.
func TestPath_EscapesEachIdentifier(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"bare base", Path(PathStatus), PathStatus},
		{"object key", Path(PathObjectTags, "bucket/a b?.txt"), "/admin/api/objects/tags/bucket%2Fa%20b%3F.txt"},
		{"backend", Path(PathBackends, "east 1") + SubpathDrain, "/admin/api/backends/east%201/drain"},
		{"grant", Path(PathProvGrants, "u1", "*"), "/admin/api/provisioning/grants/u1/%2A"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s: Path = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// TestTagConversions_RoundTrip verifies a set survives conversion to the wire
// shape and back, and that an empty set converts to a non-nil slice so it
// serialises as [] rather than null.
func TestTagConversions_RoundTrip(t *testing.T) {
	t.Parallel()
	in := []core.Tag{{Key: "a", Value: "1"}, {Key: "b", Value: "x y"}}
	if got := CoreTags(ObjectTagsFrom(in)); !slices.Equal(got, in) {
		t.Errorf("round trip = %+v, want %+v", got, in)
	}
	if ObjectTagsFrom(nil) == nil {
		t.Error("ObjectTagsFrom(nil) = nil, want an empty slice")
	}
}
