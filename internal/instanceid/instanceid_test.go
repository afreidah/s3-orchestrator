// -------------------------------------------------------------------------------
// Instance Identity Tests
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package instanceid

import (
	"strings"
	"testing"
)

// TestNew_FormatAndUniqueness verifies the produced identifier follows the
// hostname-XXXXXXXX shape and that two consecutive calls produce different
// suffixes (the random 8 hex chars must collide-resist within a process).
func TestNew_FormatAndUniqueness(t *testing.T) {
	t.Parallel()

	id1 := mustNew(t)
	id2 := mustNew(t)
	if id1 == id2 {
		t.Fatalf("two consecutive New() calls returned identical IDs: %q", id1)
	}
	checkIDFormat(t, id1)
	checkIDFormat(t, id2)
}

// mustNew calls New and fails the test on error.
func mustNew(t *testing.T) ID {
	t.Helper()
	id, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return id
}

// checkIDFormat asserts an identifier follows the hostname-XXXXXXXX shape
// (non-empty host, 8 lowercase-hex suffix).
func checkIDFormat(t *testing.T, id ID) {
	t.Helper()
	s := id.String()
	idx := strings.LastIndex(s, "-")
	if idx == -1 {
		t.Errorf("id %q missing - separator", s)
		return
	}
	if s[:idx] == "" {
		t.Errorf("id %q has empty host prefix", s)
	}
	suffix := s[idx+1:]
	if len(suffix) != 8 {
		t.Errorf("id %q suffix len = %d, want 8", s, len(suffix))
	}
	if !isLowerHex(suffix) {
		t.Errorf("id %q suffix contains non-hex characters", s)
	}
}

// isLowerHex reports whether every byte in s is a lowercase hex digit.
func isLowerHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
