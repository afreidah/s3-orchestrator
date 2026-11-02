// -------------------------------------------------------------------------------
// Cycle Tests
//
// Author: Alex Freidah
//
// Pins what one pass reports: the cycle metric always, a completion entry only
// for a pass that failed or had work, a quota refresh only after a pass that
// changed something, and the request ID the caller's context carried. The
// tests capture the default logger, so none run in parallel.
// -------------------------------------------------------------------------------

package cycle

import (
	"context"
	"errors"
	"testing"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit/audittest"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
)

// refresher counts quota refreshes and fails them with err.
type refresher struct {
	calls int
	err   error
}

// UpdateQuotaMetrics counts one refresh.
func (r *refresher) UpdateQuotaMetrics(context.Context) error {
	r.calls++
	return r.err
}

// runPass runs one pass under workload wl that returns sum and err, and
// returns the audit entries it wrote.
func runPass(t *testing.T, ctx context.Context, p Pass, sum batch.Summary, err error) []audittest.Entry {
	t.Helper()
	return audittest.Capture(t, func() {
		_, _ = Run(ctx, p, func(context.Context) (batch.Summary, error) { return sum, err })
	})
}

// TestRun_ReportsByWhatThePassDid pins which passes write a completion entry
// and refresh the gauges: an idle pass does neither, a pass whose every item
// was skipped is still reported, and only a pass that completed an item
// refreshes.
func TestRun_ReportsByWhatThePassDid(t *testing.T) {
	boom := errors.New("listing failed")
	tests := []struct {
		name        string
		sum         batch.Summary
		err         error
		wantOutcome string
		wantEntry   bool
		wantRefresh int
	}{
		{"idle", batch.Summary{}, nil, batch.OutcomeEmpty, false, 0},
		{"every item skipped", batch.Summary{Planned: 2, Skipped: 2}, nil, batch.OutcomeEmpty, true, 0},
		{"work deferred", batch.Summary{Deferred: 3}, nil, batch.OutcomeEmpty, true, 0},
		{"an item completed", batch.Summary{Planned: 1, Attempted: 1, Succeeded: 1}, nil, batch.OutcomeSuccess, true, 1},
		{"failed before a tally", batch.Summary{}, boom, batch.OutcomeError, true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wl := audit.Workload("cycle_test_" + tt.name)
			r := &refresher{}
			metric := telemetry.WorkerCyclesTotal.WithLabelValues(string(wl), tt.wantOutcome)
			before := promtest.ToFloat64(metric)

			entries := audittest.Named(runPass(t, context.Background(), Pass{Workload: wl, Refresh: r}, tt.sum, tt.err), string(wl)+".complete")

			if got := promtest.ToFloat64(metric) - before; got != 1 {
				t.Errorf("cycles{%s} moved by %v, want 1", tt.wantOutcome, got)
			}
			if (len(entries) == 1) != tt.wantEntry {
				t.Errorf("completion entries = %d, want one: %v", len(entries), tt.wantEntry)
			}
			if r.calls != tt.wantRefresh {
				t.Errorf("refreshes = %d, want %d", r.calls, tt.wantRefresh)
			}
		})
	}
}

// TestRun_RefreshFailureIsNotThePasss verifies a failed quota refresh leaves
// the pass's own result and error alone.
func TestRun_RefreshFailureIsNotThePasss(t *testing.T) {
	r := &refresher{err: errors.New("metrics down")}
	sum, err := Run(context.Background(), Pass{Workload: "cycle_test_refresh", Refresh: r},
		func(context.Context) (batch.Summary, error) { return batch.Summary{Succeeded: 2}, nil })
	if err != nil || sum.Succeeded != 2 || r.calls != 1 {
		t.Errorf("Run = %+v, %v after %d refreshes; want the pass's result after one", sum, err, r.calls)
	}
}

// TestRun_KeepsTheCallersRequestID verifies a pass started under a request is
// traced to it, and a pass started under none gets an ID of its own.
func TestRun_KeepsTheCallersRequestID(t *testing.T) {
	work := batch.Summary{Planned: 1, Attempted: 1, Succeeded: 1}
	p := Pass{Workload: "cycle_test_request_id"}

	entries := runPass(t, audit.WithRequestID(context.Background(), "req-1"), p, work, nil)
	if len(entries) != 1 || entries[0]["request_id"] != "req-1" {
		t.Errorf("entries = %v, want one under the caller's request_id", entries)
	}
	entries = runPass(t, context.Background(), p, work, nil)
	if len(entries) != 1 || entries[0]["request_id"] == "" || entries[0]["request_id"] == nil {
		t.Errorf("entries = %v, want one under a minted request_id", entries)
	}
}
