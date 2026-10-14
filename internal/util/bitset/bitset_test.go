// -------------------------------------------------------------------------------
// bitset - Tests
//
// Author: Alex Freidah
//
// Rendering, lookup, validation and the mask tests over a small vocabulary.
// Validation matters most: a table that gives two names one bit makes setting
// either set both.
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

// TestVocabulary_Lookup finds a name's bit, refuses an unknown name, and lists
// every name in vocabulary order.
func TestVocabulary_Lookup(t *testing.T) {
	t.Parallel()
	if bit, ok := testVocab.Lookup("b"); !ok || bit != flagB {
		t.Errorf("Lookup(b) = %d, %t; want %d, true", bit, ok, flagB)
	}
	if _, ok := testVocab.Lookup("z"); ok {
		t.Error("Lookup(z) found a bit")
	}
	if got := testVocab.AllNames(); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("AllNames = %v, want [a b c]", got)
	}
}

// TestMaskTests covers the three ways a set is compared to a mask, including
// the empty set and the empty mask at each edge.
func TestMaskTests(t *testing.T) {
	t.Parallel()
	ab := flagA | flagB
	for _, tc := range []struct {
		name      string
		got, want bool
	}{
		{"Has all", Has(ab, flagA|flagB), true},
		{"Has one missing", Has(ab, flagA|flagC), false},
		{"Has nothing wanted", Has(testFlags(0), 0), true},
		{"Has from empty", Has(testFlags(0), flagA), false},
		{"Any overlap", Any(ab, flagB|flagC), true},
		{"Any disjoint", Any(ab, flagC), false},
		{"Any empty mask", Any(ab, 0), false},
		{"Within inside", Within(flagA, ab), true},
		{"Within outside", Within(flagA|flagC, ab), false},
		{"Within empty set", Within(testFlags(0), 0), true},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %t, want %t", tc.name, tc.got, tc.want)
		}
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
