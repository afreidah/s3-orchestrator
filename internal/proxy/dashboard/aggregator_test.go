// -------------------------------------------------------------------------------
// DashboardAggregator Tests
//
// Author: Alex Freidah
//
// Tests for dashboard data aggregation: successful assembly from the fleet
// snapshot, a snapshot failure, and the compression views.
//
// Drives the aggregator against the generated FleetView and UsageReader mocks,
// so a test states only the reads it cares about and any unstubbed call fails
// the test rather than returning a silent zero value.
// -------------------------------------------------------------------------------

package dashboard

import (
	"errors"
	"maps"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestAggregator_Success assembles the dashboard from the fleet snapshot and
// the tracker's cycle usage.
func TestAggregator_Success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)

	usage := NewMockUsageReader(ctrl)
	usage.EXPECT().GetLimits().Return(map[string]core.UsageLimits{}).AnyTimes()
	cycle := core.UsagePeriod{ResetDay: 15}.Cycle(time.Now())
	usage.EXPECT().CycleUsage().Return(map[string]core.CycleUsage{"b1": {
		Usage: core.Usage{UsageStat: core.UsageStat{APIRequests: 10}, Pools: core.PoolUsage{core.PoolAll: 10}},
		Cycle: cycle,
	}})
	snap := snapshotOf{snap: &metrics.FleetSnapshot{
		Quota: map[string]core.QuotaStat{"b1": {BytesUsed: 100}},
		Ledger: core.LedgerStats{"b1": {
			Objects:    5,
			Plaintext:  7,
			Compressed: core.CompressionStat{Objects: 2, LogicalBytes: 1000, StoredBytes: 250},
		}},
		Multipart: map[string]int64{"b1": 1},
		Coverage:  core.CoverageStat{OldestUnverifiedAge: time.Hour, NeverVerified: 3, Deferred: 4},
	}}

	da := New(&Deps{Usage: usage, Fleet: newStubFleet(ctrl, "b1"), Snapshots: snap})
	data, err := da.GetStatus(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if data.QuotaStats["b1"].BytesUsed != 100 {
		t.Errorf("BytesUsed = %d, want 100", data.QuotaStats["b1"].BytesUsed)
	}
	if u := data.UsageStats["b1"]; u.APIRequests != 10 || u.Pools[core.PoolAll] != 10 {
		t.Errorf("UsageStats = %+v, want b1 with 10 requests on the wildcard pool", data.UsageStats)
	}
	if data.UsageCycles["b1"] != cycle {
		t.Errorf("UsageCycles = %+v, want b1 in %+v", data.UsageCycles, cycle)
	}
	if !maps.Equal(data.Ledger, snap.snap.Ledger) {
		t.Errorf("Ledger = %+v, want the snapshot's %+v", data.Ledger, snap.snap.Ledger)
	}
	if data.OldestUnverifiedAge != time.Hour || data.NeverVerifiedCopies != 3 || data.DeferredCopies != 4 {
		t.Errorf("coverage = %v, %d, %d; want the snapshot's 1h, 3, 4",
			data.OldestUnverifiedAge, data.NeverVerifiedCopies, data.DeferredCopies)
	}
	if len(data.BackendOrder) != 1 || data.BackendOrder[0] != "b1" {
		t.Errorf("BackendOrder = %v, want [b1]", data.BackendOrder)
	}
}

// TestAggregator_SnapshotFailureFailsGetStatus fails the status rather than
// report zeroes when a store read behind the snapshot failed.
func TestAggregator_SnapshotFailureFailsGetStatus(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	da := New(&Deps{
		Usage: stubUsage(ctrl), Fleet: NewMockFleetView(ctrl), Snapshots: snapshotOf{nil, errors.New("db down")},
	})
	if _, err := da.GetStatus(t.Context()); err == nil {
		t.Fatal("GetStatus succeeded without a snapshot")
	}
}

// TestData_CompressionViews covers what the dashboard gates on. The config flag
// says whether new writes will be encoded; these say whether anything already
// is, and the two disagree in both directions that matter. A fleet that has
// just turned compression off still holds everything it compressed, and that is
// exactly when an operator wants to see the savings and reach the unwind.
func TestData_CompressionViews(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		stats     map[string]core.CompressionStat
		wantAny   bool
		wantSaved int64
		wantTotal core.CompressionStat
	}{
		{
			name:    "nothing compressed",
			stats:   map[string]core.CompressionStat{},
			wantAny: false,
		},
		{
			name: "one backend",
			stats: map[string]core.CompressionStat{
				"b1": {Objects: 2, LogicalBytes: 1000, StoredBytes: 250},
			},
			wantAny:   true,
			wantSaved: 750,
			wantTotal: core.CompressionStat{Objects: 2, LogicalBytes: 1000, StoredBytes: 250},
		},
		{
			name: "summed across backends",
			stats: map[string]core.CompressionStat{
				"b1": {Objects: 2, LogicalBytes: 1000, StoredBytes: 250},
				"b2": {Objects: 1, LogicalBytes: 500, StoredBytes: 400},
			},
			wantAny:   true,
			wantSaved: 750,
			wantTotal: core.CompressionStat{Objects: 3, LogicalBytes: 1500, StoredBytes: 650},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := &Data{Ledger: compressedLedger(tt.stats)}

			if got := d.HasCompressedData(); got != tt.wantAny {
				t.Errorf("HasCompressedData() = %v, want %v", got, tt.wantAny)
			}
			if got := d.CompressionSaved("b1"); got != tt.wantSaved {
				t.Errorf("CompressionSaved(b1) = %d, want %d", got, tt.wantSaved)
			}
			if got := d.CompressionTotals(); got != tt.wantTotal {
				t.Errorf("CompressionTotals() = %+v, want %+v", got, tt.wantTotal)
			}
		})
	}
}

// TestData_CompressionSavedUnknownBackend checks a backend holding nothing
// encoded reports zero rather than a stale figure from another backend.
func TestData_CompressionSavedUnknownBackend(t *testing.T) {
	t.Parallel()
	d := &Data{Ledger: compressedLedger(map[string]core.CompressionStat{
		"b1": {Objects: 1, LogicalBytes: 900, StoredBytes: 100},
	})}
	if got := d.CompressionSaved("b2"); got != 0 {
		t.Errorf("CompressionSaved(b2) = %d, want 0", got)
	}
}

// compressedLedger is a ledger whose backends hold only the given encoded
// copies.
func compressedLedger(stats map[string]core.CompressionStat) core.LedgerStats {
	ledger := make(core.LedgerStats, len(stats))
	for name, s := range stats {
		ledger[name] = core.LedgerStat{Compressed: s}
	}
	return ledger
}
