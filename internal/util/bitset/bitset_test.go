// -------------------------------------------------------------------------------
// bitset - Tests
//
// Author: Alex Freidah
//
// Rendering and validation over a small vocabulary. Validation matters most: a
// table that gives two names one bit makes setting either set both.
// -------------------------------------------------------------------------------

package bitset

import (
	"slices"
	"testing"
)

// testFlags is a flag type for the tests.
type testFlags uint64

const (
	flagA testFlags = 1 << iota
	flagB
	flagC
)

var testVocab = Vocabulary[testFlags]{{flagA, "a"}, {flagB, "b"}, {flagC, "c"}}

// TestVocabulary_Render covers names and join, in vocabulary order whatever
// order the bits were set in.
func TestVocabulary_Render(t *testing.T) {
	t.Parallel()
	if got := testVocab.Names(flagC | flagA); !slices.Equal(got, []string{"a", "c"}) {
		t.Errorf("Names = %v, want [a c]", got)
	}
	if got := testVocab.Join(flagC | flagA); got != "a,c" {
		t.Errorf("Join = %q, want a,c", got)
	}
	if got := testVocab.Join(0); got != "" {
		t.Errorf("Join(0) = %q, want empty", got)
	}
}

// TestVocabulary_Validate accepts a sound table and names each kind of fault.
func TestVocabulary_Validate(t *testing.T) {
	t.Parallel()
	if err := testVocab.Validate(); err != nil {
		t.Fatalf("Validate on a sound vocabulary: %v", err)
	}
	for name, v := range map[string]Vocabulary[testFlags]{
		"zero bit":    {{0, "a"}},
		"two bits":    {{flagA | flagB, "ab"}},
		"shared bit":  {{flagA, "a"}, {flagA, "b"}},
		"shared name": {{flagA, "a"}, {flagB, "a"}},
	} {
		if err := v.Validate(); err == nil {
			t.Errorf("%s: Validate accepted it", name)
		}
	}
}
