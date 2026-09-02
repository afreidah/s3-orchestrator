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
	"errors"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
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

// requestCap builds limits with a single wildcard request pool, the shape a
// bare api_request_limit desugars into.
func requestCap(t *testing.T, limit int64) core.UsageLimits {
	t.Helper()
	lim, err := core.NewUsageLimits(0, 0, core.SingleRequestPool(limit), nil)
	if err != nil {
		t.Fatalf("build limits: %v", err)
	}
	return lim
}

// byteCaps builds limits that bound bytes only, leaving requests unmetered.
func byteCaps(t *testing.T, egress, ingress int64) core.UsageLimits {
	t.Helper()
	lim, err := core.NewUsageLimits(egress, ingress, nil, nil)
	if err != nil {
		t.Fatalf("build limits: %v", err)
	}
	return lim
}

// TestAllow_UnlimitedBackendAdmitsEverything checks the common case: a backend
// with no configured limits never refuses, so a deployment that has not opted
// into metering behaves exactly as it did before admission existed.
func TestAllow_UnlimitedBackendAdmitsEverything(t *testing.T) {
	t.Parallel()
	rec, _ := newTestRecorder(t, []string{"b1"}, nil)

	if !rec.Allow("b1", []s3op.Operation{s3op.PutObject}, 1<<40, 1<<40) {
		t.Error("a backend with no limits refused an operation")
	}
}

// TestAllow_RefusesPastEachLimit checks all three dimensions are enforced
// independently. A backend can have room for the bytes and not the requests,
// or the other way round, and either one has to be able to refuse on its own.
func TestAllow_RefusesPastEachLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		limits        core.UsageLimits
		spent         core.UsageStat
		spentPools    core.PoolUsage
		ops           []s3op.Operation
		egress, ingre int64
		want          bool
	}{
		{
			name:       "api requests exhausted",
			limits:     requestCap(t, 10),
			spent:      core.UsageStat{APIRequests: 10},
			spentPools: core.PoolUsage{core.PoolAll: 10},
			// One more request is one too many.
			ops:  []s3op.Operation{s3op.PutObject},
			want: false,
		},
		{
			name:   "egress exhausted",
			limits: byteCaps(t, 100, 0),
			spent:  core.UsageStat{EgressBytes: 60},
			egress: 50,
			want:   false,
		},
		{
			name:   "ingress exhausted",
			limits: byteCaps(t, 0, 100),
			spent:  core.UsageStat{IngressBytes: 60},
			ingre:  50,
			want:   false,
		},
		{
			name:   "egress limit does not refuse an ingress-only operation",
			limits: byteCaps(t, 100, 0),
			spent:  core.UsageStat{EgressBytes: 100},
			ingre:  50,
			want:   true,
		},
		{
			name:   "operation that still fits",
			limits: byteCaps(t, 100, 0),
			spent:  core.UsageStat{EgressBytes: 40},
			ops:    []s3op.Operation{s3op.GetObject},
			egress: 50,
			want:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec, tracker := newTestRecorder(t, []string{"b1"}, map[string]core.UsageLimits{"b1": tt.limits})
			tracker.SetBaseline("b1", tt.spent, tt.spentPools)

			if got := rec.Allow("b1", tt.ops, tt.egress, tt.ingre); got != tt.want {
				t.Errorf("Allow(%v, %d, %d) = %v, want %v", tt.ops, tt.egress, tt.ingre, got, tt.want)
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
		"b1": byteCaps(t, 100, 0),
	})
	read := []s3op.Operation{s3op.GetObject}

	if !rec.Allow("b1", read, 60, 0) {
		t.Fatal("first operation refused with a full budget")
	}
	rec.Egress(s3op.GetObject, "b1", 60)

	if rec.Allow("b1", read, 60, 0) {
		t.Error("second operation admitted; the spend from the first was not visible to Allow")
	}
	if !rec.Allow("b1", read, 40, 0) {
		t.Error("an operation that fits in what remains was refused")
	}
}

// TestRecordMethods_ChargeTheOperationsPools is what makes the class split
// work end to end: each charge carries its operation, so it lands on the pools
// that operation belongs to rather than on a single counter. A method that
// charged the wrong pool would refuse the wrong work later.
func TestRecordMethods_ChargeTheOperationsPools(t *testing.T) {
	t.Parallel()
	lim, err := core.NewUsageLimits(0, 0, []core.PoolSpec{
		{Name: "class_a", Operations: []string{string(s3op.PutObject), string(s3op.UploadPart)}, Limit: 100},
		{Name: "class_b", Operations: []string{string(s3op.GetObject), string(s3op.ListObjects)}, Limit: 100},
	}, nil)
	if err != nil {
		t.Fatalf("build limits: %v", err)
	}

	cb := counter.NewLocalCounterBackend([]string{"b1"})
	tracker := counter.NewUsageTracker(cb, map[string]core.UsageLimits{"b1": lim})
	var ops []string
	rec := New(tracker, func(operation, _ string, _ time.Time, _ error) {
		ops = append(ops, operation)
	})

	rec.APICall(s3op.PutObject, "b1")                                         // class_a
	rec.APICalls(s3op.ListObjects, "b1", 3)                                   // class_b x3, one paginated walk
	rec.Egress(s3op.GetObject, "b1", 500)                                     // class_b
	rec.Ingress(s3op.UploadPart, "b1", 700)                                   // class_a
	rec.PutSuccess(s3op.PutObject, "b1", 900, time.Now())                     // class_a + metric
	rec.GetSuccess(s3op.GetObject, "b1", 100, time.Now())                     // class_b + metric
	rec.OperationFailed(s3op.PutObject, "b1", time.Now(), errors.New("boom")) // class_a + metric

	if got := cb.LoadPool("b1", "class_a"); got != 4 {
		t.Errorf("class_a = %d, want 4 (APICall, Ingress, PutSuccess, OperationFailed)", got)
	}
	if got := cb.LoadPool("b1", "class_b"); got != 5 {
		t.Errorf("class_b = %d, want 5 (3 list pages, Egress, GetSuccess)", got)
	}

	all := cb.LoadAll("b1")
	if all.APIRequests != 9 {
		t.Errorf("api_requests = %d, want 9; every call counts toward the total", all.APIRequests)
	}
	if all.EgressBytes != 600 || all.IngressBytes != 1600 {
		t.Errorf("bytes = %d egress / %d ingress, want 600 / 1600", all.EgressBytes, all.IngressBytes)
	}

	// The three combo helpers emit the operation metric; the bare charges do
	// not, which is what keeps a failover attempt from logging a second
	// observation for one client operation.
	if len(ops) != 3 {
		t.Errorf("operation metric emitted %d times (%v), want 3", len(ops), ops)
	}
}

// TestOperation_EmitsTheMetricWithoutCharging pins the split between the two
// surfaces: the metric can be recorded on its own, for the paths that charge
// per attempt but observe once per operation.
func TestOperation_EmitsTheMetricWithoutCharging(t *testing.T) {
	t.Parallel()
	cb := counter.NewLocalCounterBackend([]string{"b1"})
	tracker := counter.NewUsageTracker(cb, nil)
	var got string
	rec := New(tracker, func(operation, _ string, _ time.Time, _ error) { got = operation })

	rec.Operation(s3op.HeadObject, "b1", time.Now(), nil)

	if got != string(s3op.HeadObject) {
		t.Errorf("metric operation = %q, want HeadObject", got)
	}
	if n := cb.LoadAll("b1").APIRequests; n != 0 {
		t.Errorf("api_requests = %d, want 0; Operation records no charge", n)
	}
}

// TestAllow_UnknownBackendAdmits checks a name with no limits entry is
// admitted rather than refused. A backend added at runtime has no limits until
// the next config load, and refusing everything against it would take it out
// of service for the gap.
func TestAllow_UnknownBackendAdmits(t *testing.T) {
	t.Parallel()
	rec, _ := newTestRecorder(t, []string{"b1"}, map[string]core.UsageLimits{
		"b1": byteCaps(t, 1, 0),
	})

	if !rec.Allow("b-unknown", []s3op.Operation{s3op.GetObject}, 1<<30, 0) {
		t.Error("a backend with no limits entry was refused")
	}
}
