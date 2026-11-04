// -------------------------------------------------------------------------------
// commalist Tests
//
// Author: Alex Freidah
//
// Pins the trimming, the dropped empty entries, and nil for a blank value, and
// fuzzes that no entry ever comes back empty or untrimmed.
// -------------------------------------------------------------------------------

package commalist

import (
	"slices"
	"strings"
	"testing"
)

// TestSplit verifies entries come back trimmed and in order, with empty ones
// dropped and a blank value returning nil.
func TestSplit(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		"":                nil,
		" , ,":            nil,
		"a":               {"a"},
		" a , b ,, c ":    {"a", "b", "c"},
		"X-Amz-Meta-One,": {"X-Amz-Meta-One"},
	}
	for in, want := range cases {
		if got := Split(in); !slices.Equal(got, want) {
			t.Errorf("Split(%q) = %q, want %q", in, got, want)
		}
	}
}

// FuzzSplit pins that no entry is empty or carries surrounding space, either
// of which silently fails a CORS rule the operator wrote correctly.
func FuzzSplit(f *testing.F) {
	f.Add("content-type")
	f.Add("  Content-Type ,, x-amz-date  ")
	f.Add(",")

	f.Fuzz(func(t *testing.T, v string) {
		for _, entry := range Split(v) {
			if entry == "" || entry != strings.TrimSpace(entry) {
				t.Errorf("Split(%q) produced %q", v, entry)
			}
		}
	})
}
