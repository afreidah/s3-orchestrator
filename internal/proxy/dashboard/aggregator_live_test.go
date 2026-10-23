// -------------------------------------------------------------------------------
// Dashboard Aggregator - Live State Tests
//
// Author: Alex Freidah
//
// Covers the fields the aggregator fills from the running fleet rather than
// the store: which backends are draining and how far along, and which are
// refusing any call. Both are best-effort, so the interesting
// cases are the ones where a dependency is absent or errors.
//
// These exercise the aggregator directly against the DashboardStore and
// FleetView mocks instead of standing up a full proxy stack.
// -------------------------------------------------------------------------------

package dashboard

import (
	"errors"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/proxy/backendstate"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// newStubStore returns a DashboardStore mock whose reads the aggregator makes
// succeed with an empty result, so each test states only the calls it cares
// about.
func newStubStore(ctrl *gomock.Controller) *storetest.MockDashboardStore {
	store := storetest.NewMockDashboardStore(ctrl)
	store.EXPECT().ListDirectoryChildren(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&core.DirectoryListResult{}, nil).AnyTimes()
	return store
}

// emptySnapshot is a complete fleet snapshot of an empty fleet: every map
// present and empty, so the aggregator does not treat it as incomplete.
func emptySnapshot() *metrics.FleetSnapshot {
	return &metrics.FleetSnapshot{
		Quota:     map[string]core.QuotaStat{},
		Ledger:    core.LedgerStats{},
		Multipart: map[string]int64{},
	}
}

// expectSnapshot has fleet serve snap as the fleet snapshot, however many
// times the aggregator asks.
func expectSnapshot(fleet *MockFleetView, snap *metrics.FleetSnapshot) {
	fleet.EXPECT().FleetSnapshot(gomock.Any()).Return(snap, nil).AnyTimes()
}

// newStubFleet returns a FleetView serving snap over an idle fleet with no
// backends.
func newStubFleet(ctrl *gomock.Controller, snap *metrics.FleetSnapshot) *MockFleetView {
	fleet := NewMockFleetView(ctrl)
	expectSnapshot(fleet, snap)
	fleet.EXPECT().BackendOrder().Return(nil).AnyTimes()
	return fleet
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// stubUsage returns a UsageReader mock with no configured limits, which is
// what every test here wants: the limits themselves are the store's business.
func stubUsage(ctrl *gomock.Controller) *MockUsageReader {
	usage := NewMockUsageReader(ctrl)
	usage.EXPECT().GetLimits().Return(map[string]core.UsageLimits{}).AnyTimes()
	usage.EXPECT().CycleUsage().Return(map[string]core.CycleUsage{}).AnyTimes()
	return usage
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestDecorateLiveState_RecordsRefusedCalls asserts a backend refusing only GET
// is recorded with exactly that call refused.
func TestDecorateLiveState_RecordsRefusedCalls(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)

	fleet := NewMockFleetView(ctrl)
	expectSnapshot(fleet, emptySnapshot())
	fleet.EXPECT().BackendOrder().Return([]string{"b1"}).AnyTimes()
	fleet.EXPECT().BackendFlags(gomock.Any()).Return(core.BackendFlags(0)).AnyTimes()
	fleet.EXPECT().ServedCalls("b1").Return(core.AllCalls &^ core.CallGetObject).AnyTimes()

	agg := New(newStubStore(ctrl), stubUsage(ctrl), nil, []string{"b1"}, fleet, nil)

	data, err := agg.GetData(t.Context())
	if err != nil {
		t.Fatalf("GetData: %v", err)
	}
	if got := data.RefusedCalls["b1"]; got != core.CallGetObject {
		t.Errorf("b1 refused calls = %s, want GetObject", got)
	}
}

// TestDecorateLiveState_HealthyBackendAbsent asserts a healthy backend does not
// appear in the map at all, rather than appearing with a false value.
func TestDecorateLiveState_HealthyBackendAbsent(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)

	fleet := NewMockFleetView(ctrl)
	expectSnapshot(fleet, emptySnapshot())
	fleet.EXPECT().BackendOrder().Return([]string{"b1"}).AnyTimes()
	fleet.EXPECT().BackendFlags(gomock.Any()).Return(core.BackendFlags(0)).AnyTimes()
	fleet.EXPECT().ServedCalls("b1").Return(core.AllCalls).AnyTimes()

	agg := New(newStubStore(ctrl), stubUsage(ctrl), nil, []string{"b1"}, fleet, nil)

	data, err := agg.GetData(t.Context())
	if err != nil {
		t.Fatalf("GetData: %v", err)
	}
	if _, present := data.RefusedCalls["b1"]; present {
		t.Error("a backend serving every call should be absent from RefusedCalls, not present-and-empty")
	}
}

// TestDecorateLiveState_ReportsDrainProgress covers the draining path: only
// backends reporting as draining are queried for progress.
func TestDecorateLiveState_ReportsDrainProgress(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)

	fleet := NewMockFleetView(ctrl)
	expectSnapshot(fleet, emptySnapshot())
	fleet.EXPECT().BackendOrder().Return([]string{"draining", "idle"}).AnyTimes()
	fleet.EXPECT().BackendFlags("draining").Return(core.FlagDraining).AnyTimes()
	fleet.EXPECT().BackendFlags("idle").Return(core.BackendFlags(0)).AnyTimes()
	fleet.EXPECT().ServedCalls(gomock.Any()).Return(core.AllCalls).AnyTimes()

	drainReader := NewMockDrainProgressReader(ctrl)
	// Only the draining backend is asked; querying the idle one would be a bug.
	drainReader.EXPECT().GetDrainProgress(gomock.Any(), "draining").
		Return(&backendstate.DrainProgress{Active: true, ObjectsRemaining: 6, ObjectsMoved: 4}, nil).Times(1)

	agg := New(newStubStore(ctrl), stubUsage(ctrl), nil, nil, fleet, drainReader)

	data, err := agg.GetData(t.Context())
	if err != nil {
		t.Fatalf("GetData: %v", err)
	}
	got, ok := data.DrainingBackends["draining"]
	if !ok {
		t.Fatal("draining backend missing from DrainingBackends")
	}
	if !got.Active || got.ObjectsMoved != 4 || got.ObjectsRemaining != 6 {
		t.Errorf("progress = %+v, want active with 4 moved / 6 remaining", got)
	}
	if _, ok := data.DrainingBackends["idle"]; ok {
		t.Error("an idle backend must not appear in DrainingBackends")
	}
}

// TestDecorateLiveState_DrainProgressErrorIsSkipped asserts a backend whose
// progress cannot be read is omitted rather than failing the whole dashboard.
func TestDecorateLiveState_DrainProgressErrorIsSkipped(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)

	fleet := NewMockFleetView(ctrl)
	expectSnapshot(fleet, emptySnapshot())
	fleet.EXPECT().BackendOrder().Return([]string{"b1"}).AnyTimes()
	fleet.EXPECT().BackendFlags(gomock.Any()).Return(core.FlagDraining).AnyTimes()
	fleet.EXPECT().ServedCalls(gomock.Any()).Return(core.AllCalls).AnyTimes()

	drainReader := NewMockDrainProgressReader(ctrl)
	drainReader.EXPECT().GetDrainProgress(gomock.Any(), gomock.Any()).
		Return(nil, errors.New("boom")).AnyTimes()

	agg := New(newStubStore(ctrl), stubUsage(ctrl), nil, nil, fleet, drainReader)

	data, err := agg.GetData(t.Context())
	if err != nil {
		t.Fatalf("GetData must not fail because one drain progress read did: %v", err)
	}
	if len(data.DrainingBackends) != 0 {
		t.Errorf("DrainingBackends = %v, want empty when progress could not be read", data.DrainingBackends)
	}
}

// TestDecorateLiveState_NoDrainManager covers a deployment with no drain
// manager: the live-state maps are still initialised rather than nil.
func TestDecorateLiveState_NoDrainManager(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	fleet := NewMockFleetView(ctrl)
	expectSnapshot(fleet, emptySnapshot())
	fleet.EXPECT().BackendOrder().Return([]string{"b1"}).AnyTimes()
	fleet.EXPECT().BackendFlags(gomock.Any()).Return(core.FlagDraining).AnyTimes()
	fleet.EXPECT().ServedCalls(gomock.Any()).Return(core.AllCalls).AnyTimes()

	agg := New(newStubStore(ctrl), stubUsage(ctrl), nil, nil, fleet, nil)
	data, err := agg.GetData(t.Context())
	if err != nil {
		t.Fatalf("GetData: %v", err)
	}
	if data.DrainingBackends == nil || data.RefusedCalls == nil {
		t.Error("live-state maps must be initialised even with no drain manager")
	}
}
