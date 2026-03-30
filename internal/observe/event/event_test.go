// -------------------------------------------------------------------------------
// Event Tests - Filter Matching
//
// Author: Alex Freidah
//
// Tests for event type wildcard matching used by notification endpoint config.
// -------------------------------------------------------------------------------

package event

import "testing"

func TestMatchesFilter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		eventType string
		patterns  []string
		want      bool
	}{
		{"s3:ObjectCreated:Put", []string{"s3:ObjectCreated:Put"}, true},
		{"s3:ObjectCreated:Put", []string{"s3:ObjectCreated:*"}, true},
		{"s3:ObjectCreated:Put", []string{"s3:*"}, true},
		{"s3:ObjectCreated:Put", []string{"*"}, true},
		{"s3:ObjectCreated:Put", []string{"s3:ObjectRemoved:*"}, false},
		{"backend.circuit.opened", []string{"backend.circuit.*"}, true},
		{"backend.circuit.opened", []string{"backend.*"}, true},
		{"backend.circuit.opened", []string{"integrity.*"}, false},
		{"backend.circuit.opened", []string{"backend.circuit.opened", "backend.circuit.closed"}, true},
		{"s3:ObjectCreated:Put", []string{}, false},
		{"s3:ObjectCreated:Put", nil, false},
	}

	for _, tt := range tests {
		got := MatchesFilter(tt.eventType, tt.patterns)
		if got != tt.want {
			t.Errorf("MatchesFilter(%q, %v) = %v, want %v", tt.eventType, tt.patterns, got, tt.want)
		}
	}
}
