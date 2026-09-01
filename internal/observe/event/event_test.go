// -------------------------------------------------------------------------------
// Event Tests - Filter Matching
//
// Author: Alex Freidah
//
// Tests for event type wildcard matching used by notification endpoint config.
// -------------------------------------------------------------------------------

package event

import (
	"fmt"
	"maps"
	"testing"
)

// TestPublish_NoNotifierIsANoOp asserts an unconfigured deployment can emit
// freely. Every emit site calls Publish unconditionally, so a nil hook that
// panicked would take down whichever pass reached one first.
func TestPublish_NoNotifierIsANoOp(t *testing.T) {
	swapEmit(t, nil)
	Publish(ServiceStarted, "", map[string]any{"version": "test"})
}

// TestPublish_DeliversTypeSubjectAndData asserts the three fields a caller
// supplies arrive unchanged. The envelope fields are the notifier's to fill,
// so Publish must leave them empty rather than guessing.
func TestPublish_DeliversTypeSubjectAndData(t *testing.T) {
	var got Event
	swapEmit(t, func(ev Event) { got = ev })

	data := map[string]any{"backend": "backend-a", "objects_moved": 7}
	Publish(BackendDrainCompleted, "backend-a", data)

	if got.Type != BackendDrainCompleted {
		t.Errorf("Type = %q, want %q", got.Type, BackendDrainCompleted)
	}
	if got.Subject != "backend-a" {
		t.Errorf("Subject = %q, want backend-a", got.Subject)
	}
	if !maps.Equal(toComparable(got.Data), toComparable(data)) {
		t.Errorf("Data = %v, want %v", got.Data, data)
	}
	if got.ID != "" || got.Source != "" || !got.Time.IsZero() {
		t.Errorf("envelope = %+v, want it left for the notifier to fill", got)
	}
}

// toComparable renders a data map for equality, since map[string]any is not
// comparable and the values here are all scalars.
func toComparable(m map[string]any) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = fmt.Sprint(v)
	}
	return out
}

// swapEmit registers hook as the emitter for one test and clears it afterwards.
// The emitter is process-global, so a test that left one installed would leak
// into every later one.
func swapEmit(t *testing.T, hook func(Event)) {
	t.Helper()
	SetEmitter(hook)
	t.Cleanup(func() { SetEmitter(nil) })
}

// TestMatchesFilter verifies the matches filter contract.
// Asserts that MatchesFilter(, ) = , want.
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
