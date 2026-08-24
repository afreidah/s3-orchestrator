// -------------------------------------------------------------------------------
// Accounting Recorder Tests
//
// Author: Alex Freidah
//
// The Recorder is where every storage subsystem asks what an operation is
// allowed to cost and reports what it did cost. Admission and accounting sit
// on one type so a caller holding it can always ask first; these tests pin
// that Allow answers from the same counters the record methods move, since a
// check that disagreed with its own accounting would admit work the backend
// has no room for.
// -------------------------------------------------------------------------------

package accounting

import (
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// newTestRecorder builds a Recorder over a local counter backend for the named
// backends, with the given limits applied.
func newTestRecorder(t *testing.T, names []string, limits map[string]core.UsageLimits) (*Recorder, *counter.UsageTracker) {
	t.Helper()
	tracker := counter.NewUsageTracker(counter.NewLocalCounterBackend(names), nil)
	if limits != nil {
		tracker.UpdateLimits(limits)
	}
	return New(tracker, func(string, string, time.Time, error) {}), tracker
}

// TestAllow_UnlimitedBackendAdmitsEverything checks the common case: a backend
// with no configured limits never refuses, so a deployment that has not opted
// into metering behaves exactly as it did before admission existed.
func TestAllow_UnlimitedBackendAdmitsEverything(t *testing.T) {
	t.Parallel()
	rec, _ := newTestRecorder(t, []string{"b1"}, nil)

	if !rec.Allow("b1", 1_000_000, 1<<40, 1<<40) {
		t.Error("a backend with no limits refused an operation")
	}
}

// TestAllow_RefusesPastEachLimit checks all three dimensions are enforced
// independently. A backend can have room for the bytes and not the requests,
// or the other way round, and either one has to be able to refuse on its own.
func TestAllow_RefusesPastEachLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                    string
		limits                  core.UsageLimits
		spent                   core.UsageStat
		apiCalls, egress, ingre int64
		want                    bool
	}{
		{
			name:   "api requests exhausted",
			limits: core.UsageLimits{APIRequestLimit: 10},
			spent:  core.UsageStat{APIRequests: 10},
			// One more request is one too many.
			apiCalls: 1,
			want:     false,
		},
		{
			name:   "egress exhausted",
			limits: core.UsageLimits{EgressByteLimit: 100},
			spent:  core.UsageStat{EgressBytes: 60},
			egress: 50,
			want:   false,
		},
		{
			name:   "ingress exhausted",
			limits: core.UsageLimits{IngressByteLimit: 100},
			spent:  core.UsageStat{IngressBytes: 60},
			ingre:  50,
			want:   false,
		},
		{
			name:   "egress limit does not refuse an ingress-only operation",
			limits: core.UsageLimits{EgressByteLimit: 100},
			spent:  core.UsageStat{EgressBytes: 100},
			ingre:  50,
			want:   true,
		},
		{
			name:     "operation that still fits",
			limits:   core.UsageLimits{EgressByteLimit: 100},
			spent:    core.UsageStat{EgressBytes: 40},
			apiCalls: 1,
			egress:   50,
			want:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec, tracker := newTestRecorder(t, []string{"b1"}, map[string]core.UsageLimits{"b1": tt.limits})
			tracker.SetBaseline("b1", tt.spent)

			if got := rec.Allow("b1", tt.apiCalls, tt.egress, tt.ingre); got != tt.want {
				t.Errorf("Allow(%d, %d, %d) = %v, want %v", tt.apiCalls, tt.egress, tt.ingre, got, tt.want)
			}
		})
	}
}

// TestAllow_SeesWhatTheRecordMethodsSpend is the load-bearing one: admission
// has to read the same counters the accounting writes, or a pass would keep
// being admitted while spending its budget and the limit would never bite.
func TestAllow_SeesWhatTheRecordMethodsSpend(t *testing.T) {
	t.Parallel()
	rec, _ := newTestRecorder(t, []string{"b1"}, map[string]core.UsageLimits{
		"b1": {EgressByteLimit: 100},
	})

	if !rec.Allow("b1", 1, 60, 0) {
		t.Fatal("first operation refused with a full budget")
	}
	rec.Egress("b1", 60)

	if rec.Allow("b1", 1, 60, 0) {
		t.Error("second operation admitted; the spend from the first was not visible to Allow")
	}
	if !rec.Allow("b1", 1, 40, 0) {
		t.Error("an operation that fits in what remains was refused")
	}
}

// TestAllow_UnknownBackendAdmits checks a name with no limits entry is
// admitted rather than refused. A backend added at runtime has no limits until
// the next config load, and refusing everything against it would take it out
// of service for the gap.
func TestAllow_UnknownBackendAdmits(t *testing.T) {
	t.Parallel()
	rec, _ := newTestRecorder(t, []string{"b1"}, map[string]core.UsageLimits{
		"b1": {EgressByteLimit: 1},
	})

	if !rec.Allow("b-unknown", 1, 1<<30, 0) {
		t.Error("a backend with no limits entry was refused")
	}
}
