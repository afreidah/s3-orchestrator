// -------------------------------------------------------------------------------
// Dashboard Aggregator - Live State Tests
//
// Author: Alex Freidah
//
// Covers the fields the aggregator fills from the running fleet rather than
// the store: which backends are draining and how far along, and which are
// failing their circuit breaker. Both are best-effort, so the interesting
// cases are the ones where a dependency is absent or errors.
//
// These exercise the aggregator directly against a 7-method DashboardStore
// mock instead of standing up a BackendManager and the 79-method union.
// -------------------------------------------------------------------------------

package dashboard

import (
	"errors"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// newStubStore returns a DashboardStore mock whose every read succeeds with
// an empty result, so each test states only the calls it cares about.
func newStubStore(ctrl *gomock.Controller) *storetest.MockDashboardStore {
	store := storetest.NewMockDashboardStore(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).Return(map[string]core.QuotaStat{}, nil).AnyTimes()
	store.EXPECT().GetObjectCounts(gomock.Any()).Return(map[string]int64{}, nil).AnyTimes()
	store.EXPECT().GetUnverifiedObjectCounts(gomock.Any()).Return(map[string]int64{}, nil).AnyTimes()
	store.EXPECT().GetActiveMultipartCounts(gomock.Any()).Return(map[string]int64{}, nil).AnyTimes()
	store.EXPECT().OldestUnverifiedAge(gomock.Any()).Return(time.Duration(0), int64(0), nil).AnyTimes()
	store.EXPECT().CountUnencryptedLocations(gomock.Any()).Return(int64(0), nil).AnyTimes()
	store.EXPECT().CompressionStats(gomock.Any()).Return(map[string]core.CompressionStat{}, nil).AnyTimes()
	store.EXPECT().GetUsageForPeriod(gomock.Any(), gomock.Any()).Return(map[string]core.UsageStat{}, nil).AnyTimes()
	store.EXPECT().ListDirectoryChildren(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&core.DirectoryListResult{}, nil).AnyTimes()
	return store
}

// stubUsage returns a UsageReader mock with no configured limits, which is
// what every test here wants: the limits themselves are the store's business.
func stubUsage(ctrl *gomock.Controller) *MockUsageReader {
	usage := NewMockUsageReader(ctrl)
	usage.EXPECT().GetLimits().Return(map[string]core.UsageLimits{}).AnyTimes()
	return usage
}

// trippedBackend returns a circuit-breaker backend whose breaker is already
// open, which is how the aggregator recognises an unhealthy backend.
func trippedBackend(t *testing.T, ctrl *gomock.Controller) backend.ObjectBackend {
	t.Helper()
	inner := backendtest.NewMockObjectBackend(ctrl)
	inner.EXPECT().PutObject(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return("", errors.New("down")).AnyTimes()
	cb := backend.NewCircuitBreakerBackend(inner, backend.CircuitBreakerConfig{
		Name: "b1", Threshold: 1, Timeout: time.Minute,
	})
	if _, err := cb.PutObject(t.Context(), "k", nil, 0, "", nil); err == nil {
		t.Fatal("expected the seed PutObject to fail and trip the breaker")
	}
	return cb
}

// TestDecorateLiveState_MarksUnhealthyBackends asserts a backend whose breaker
// is open is reported unhealthy.
func TestDecorateLiveState_MarksUnhealthyBackends(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)

	cb := trippedBackend(t, ctrl)
	fleet := NewMockFleetView(ctrl)
	fleet.EXPECT().BackendOrder().Return([]string{"b1"}).AnyTimes()
	fleet.EXPECT().IsDraining(gomock.Any()).Return(false).AnyTimes()
	fleet.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": cb}).AnyTimes()

	agg := New(newStubStore(ctrl), stubUsage(ctrl), []string{"b1"}, fleet, nil)

	data, err := agg.GetData(t.Context())
	if err != nil {
		t.Fatalf("GetData: %v", err)
	}
	if !data.UnhealthyBackends["b1"] {
		t.Error("b1 has an open breaker but was not marked unhealthy")
	}
}

// TestDecorateLiveState_HealthyBackendAbsent asserts a healthy backend does not
// appear in the map at all, rather than appearing with a false value.
func TestDecorateLiveState_HealthyBackendAbsent(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)

	healthy := backend.NewCircuitBreakerBackend(backendtest.NewMockObjectBackend(ctrl), backend.CircuitBreakerConfig{
		Name: "b1", Threshold: 5, Timeout: time.Minute,
	})
	fleet := NewMockFleetView(ctrl)
	fleet.EXPECT().BackendOrder().Return([]string{"b1"}).AnyTimes()
	fleet.EXPECT().IsDraining(gomock.Any()).Return(false).AnyTimes()
	fleet.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": healthy}).AnyTimes()

	agg := New(newStubStore(ctrl), stubUsage(ctrl), []string{"b1"}, fleet, nil)

	data, err := agg.GetData(t.Context())
	if err != nil {
		t.Fatalf("GetData: %v", err)
	}
	if _, present := data.UnhealthyBackends["b1"]; present {
		t.Error("a healthy backend should be absent from UnhealthyBackends, not present-and-false")
	}
}

// TestDecorateLiveState_ReportsDrainProgress covers the draining path: only
// backends reporting as draining are queried for progress.
func TestDecorateLiveState_ReportsDrainProgress(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)

	fleet := NewMockFleetView(ctrl)
	fleet.EXPECT().BackendOrder().Return([]string{"draining", "idle"}).AnyTimes()
	fleet.EXPECT().IsDraining("draining").Return(true).AnyTimes()
	fleet.EXPECT().IsDraining("idle").Return(false).AnyTimes()
	fleet.EXPECT().Backends().Return(map[string]backend.ObjectBackend{}).AnyTimes()

	drainReader := NewMockDrainProgressReader(ctrl)
	// Only the draining backend is asked; querying the idle one would be a bug.
	drainReader.EXPECT().GetDrainProgress(gomock.Any(), "draining").
		Return(&drain.Progress{Active: true, ObjectsRemaining: 6, ObjectsMoved: 4}, nil).Times(1)

	agg := New(newStubStore(ctrl), stubUsage(ctrl), nil, fleet, drainReader)

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
	fleet.EXPECT().BackendOrder().Return([]string{"b1"}).AnyTimes()
	fleet.EXPECT().IsDraining(gomock.Any()).Return(true).AnyTimes()
	fleet.EXPECT().Backends().Return(map[string]backend.ObjectBackend{}).AnyTimes()

	drainReader := NewMockDrainProgressReader(ctrl)
	drainReader.EXPECT().GetDrainProgress(gomock.Any(), gomock.Any()).
		Return(nil, errors.New("boom")).AnyTimes()

	agg := New(newStubStore(ctrl), stubUsage(ctrl), nil, fleet, drainReader)

	data, err := agg.GetData(t.Context())
	if err != nil {
		t.Fatalf("GetData must not fail because one drain progress read did: %v", err)
	}
	if len(data.DrainingBackends) != 0 {
		t.Errorf("DrainingBackends = %v, want empty when progress could not be read", data.DrainingBackends)
	}
}

// TestDecorateLiveState_NilDependencies covers a deployment with no drain
// manager and one with no fleet at all: both must yield initialised empty maps
// rather than nil maps or a panic.
func TestDecorateLiveState_NilDependencies(t *testing.T) {
	t.Parallel()

	t.Run("no drain manager", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		fleet := NewMockFleetView(ctrl)
		fleet.EXPECT().BackendOrder().Return([]string{"b1"}).AnyTimes()
		fleet.EXPECT().IsDraining(gomock.Any()).Return(true).AnyTimes()
		fleet.EXPECT().Backends().Return(map[string]backend.ObjectBackend{}).AnyTimes()

		agg := New(newStubStore(ctrl), stubUsage(ctrl), nil, fleet, nil)
		data, err := agg.GetData(t.Context())
		if err != nil {
			t.Fatalf("GetData: %v", err)
		}
		if data.DrainingBackends == nil || data.UnhealthyBackends == nil {
			t.Error("live-state maps must be initialised even with no drain manager")
		}
	})

	t.Run("no fleet", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		agg := New(newStubStore(ctrl), stubUsage(ctrl), nil, nil, nil)
		data, err := agg.GetData(t.Context())
		if err != nil {
			t.Fatalf("GetData: %v", err)
		}
		if data.DrainingBackends == nil || data.UnhealthyBackends == nil {
			t.Error("live-state maps must be initialised even with no fleet")
		}
	})
}

// TestGetDataPropagatesStoreError asserts a failing store read fails the call
// rather than returning partially-populated data.
func TestGetDataPropagatesStoreError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)

	store := storetest.NewMockDashboardStore(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).Return(nil, errors.New("query failed")).AnyTimes()

	agg := New(store, stubUsage(ctrl), nil, nil, nil)
	if _, err := agg.GetData(t.Context()); err == nil {
		t.Error("expected the store error to propagate")
	}
}
