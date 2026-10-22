// -------------------------------------------------------------------------------
// Batch Summary Tests
//
// Author: Alex Freidah
//
// Covers the tally arithmetic and the outcome label each tally maps to, which
// is what the runs-total metrics and their alerts key on.
// -------------------------------------------------------------------------------

package batch

import (
	"reflect"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
)

// TestSummary_Outcome pins the label each tally reports under. The expected
// values are literals because these strings are the contract operator alert
// rules are written against.
func TestSummary_Outcome(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		sum  Summary
		want string
	}{
		{"all succeeded", Summary{Succeeded: 3}, "success"},
		{"some failed", Summary{Succeeded: 2, Failed: 1}, "partial"},
		{"all failed", Summary{Failed: 3}, "failed"},
		{"nothing to do", Summary{}, "empty"},
		{"only skipped", Summary{Skipped: 4}, "empty"},
		{"deferred only", Summary{Deferred: 5}, "empty"},
	}
	for _, tc := range cases {
		if got := tc.sum.Outcome(); got != tc.want {
			t.Errorf("%s: Outcome() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestSummary_Plus verifies every field adds, so a cycle run as several batches
// reports the same totals as one batch would.
func TestSummary_Plus(t *testing.T) {
	t.Parallel()
	a := Summary{Planned: 1, Attempted: 2, Succeeded: 3, Failed: 4, Skipped: 5, Deferred: 6, Duration: time.Second,
		Usage: audit.Usage{"b1": {APIRequests: 1, EgressBytes: 100}}}
	b := Summary{Planned: 10, Attempted: 20, Succeeded: 30, Failed: 40, Skipped: 50, Deferred: 60, Duration: time.Minute,
		Usage: audit.Usage{"b1": {APIRequests: 2, IngressBytes: 7}, "b2": {APIRequests: 1}}}
	want := Summary{Planned: 11, Attempted: 22, Succeeded: 33, Failed: 44, Skipped: 55, Deferred: 66, Duration: time.Minute + time.Second,
		Usage: audit.Usage{"b1": {APIRequests: 3, EgressBytes: 100, IngressBytes: 7}, "b2": {APIRequests: 1}}}
	if got := a.Plus(b); !reflect.DeepEqual(got, want) {
		t.Errorf("Plus = %+v, want %+v", got, want)
	}
	if len(a.Usage) != 1 || a.Usage["b1"].APIRequests != 1 {
		t.Errorf("Plus modified its receiver's usage: %+v", a.Usage)
	}
}
