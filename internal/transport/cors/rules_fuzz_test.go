// -------------------------------------------------------------------------------
// CORS Rule Fuzz Tests
//
// Author: Alex Freidah
//
// Asserts the structural invariants of origin matching rather than expected
// outputs: a pattern that admits a value must agree with the literal parts it
// was compiled from. A wildcard matcher that drifts from that grants origins
// the operator never wrote, and it does so silently, so the property is worth
// holding under adversarial input.
// -------------------------------------------------------------------------------

package cors

import (
	"strings"
	"testing"
)

// FuzzPatternMatches pins that a match implies the literal parts of the
// pattern are present in the value, and that an exact pattern admits nothing
// but itself.
func FuzzPatternMatches(f *testing.F) {
	seeds := []struct {
		pattern string
		value   string
	}{
		{"https://app.example.com", "https://app.example.com"},
		{"https://*.example.com", "https://app.example.com"},
		{"*", ""},
		{"*", "https://anything.test"},
		{"abc*bcd", "abcd"},
		{"*.example.com", "https://a.b.example.com"},
		{"https://app.*", "https://app.example.com"},
		{"", ""},
	}
	for _, s := range seeds {
		f.Add(s.pattern, s.value)
	}

	f.Fuzz(func(t *testing.T, pattern, value string) {
		p, err := compilePattern(pattern)
		if err != nil {
			if strings.Count(pattern, "*") <= 1 {
				t.Errorf("compilePattern(%q) failed with at most one wildcard: %v", pattern, err)
			}
			return
		}

		matched := p.matches(value)
		if !p.wild {
			if matched != (value == pattern) {
				t.Errorf("exact pattern %q matches(%q) = %t, want %t", pattern, value, matched, value == pattern)
			}
			return
		}
		if matched {
			assertWildcardInvariants(t, pattern, value, p)
		}
	})
}

// assertWildcardInvariants checks what a wildcard match implies about the
// value: both literal parts are present, and the value is long enough to hold
// them without the two overlapping.
func assertWildcardInvariants(t *testing.T, pattern, value string, p pattern) {
	t.Helper()
	if !strings.HasPrefix(value, p.prefix) {
		t.Errorf("pattern %q admitted %q without its prefix %q", pattern, value, p.prefix)
	}
	if !strings.HasSuffix(value, p.suffix) {
		t.Errorf("pattern %q admitted %q without its suffix %q", pattern, value, p.suffix)
	}
	if len(value) < len(p.prefix)+len(p.suffix) {
		t.Errorf("pattern %q admitted %q shorter than its literal parts", pattern, value)
	}
}

// FuzzParseHeaderList pins that the split never yields an entry the matcher
// would compare with surrounding whitespace or an empty name, either of which
// silently fails a rule the operator wrote correctly.
func FuzzParseHeaderList(f *testing.F) {
	f.Add("content-type")
	f.Add("content-type, x-amz-date")
	f.Add("  Content-Type ,, x-amz-date  ")
	f.Add("")
	f.Add(",")

	f.Fuzz(func(t *testing.T, header string) {
		for _, name := range parseHeaderList(header) {
			if name == "" {
				t.Errorf("parseHeaderList(%q) produced an empty name", header)
			}
			if name != strings.TrimSpace(name) {
				t.Errorf("parseHeaderList(%q) produced %q with surrounding space", header, name)
			}
		}
	})
}
