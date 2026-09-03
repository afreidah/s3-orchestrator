// -------------------------------------------------------------------------------
// DashboardAggregator Tests
//
// Author: Alex Freidah
//
// Tests for dashboard data aggregation: successful assembly, individual query
// errors, empty data, and directory listing edge cases.
//
// Drives the aggregator against the generated 6-method DashboardStore mock, so
// a test states only the reads it cares about and any unstubbed call fails the
// test rather than returning a silent zero value.
// -------------------------------------------------------------------------------

package dashboard

import (
	"errors"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestAggregator_Success verifies the aggregator success contract.
// Asserts that BytesUsed = , want 100.
func TestAggregator_Success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)

	store := storetest.NewMockDashboardStore(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{"b1": {BytesUsed: 100}}, nil)
	store.EXPECT().GetObjectCounts(gomock.Any()).Return(map[string]int64{"b1": 5}, nil)
	store.EXPECT().GetUnverifiedObjectCounts(gomock.Any()).Return(map[string]int64{}, nil)
	store.EXPECT().GetActiveMultipartCounts(gomock.Any()).Return(map[string]int64{"b1": 1}, nil)
	store.EXPECT().OldestUnverifiedAge(gomock.Any()).Return(2*time.Hour, int64(3), nil)
	store.EXPECT().CountUnencryptedLocations(gomock.Any()).Return(int64(7), nil)
	store.EXPECT().CompressionStats(gomock.Any()).
		Return(map[string]core.CompressionStat{"b1": {Objects: 2, LogicalBytes: 1000, StoredBytes: 250}}, nil)
	store.EXPECT().GetUsageForPeriod(gomock.Any(), gomock.Any()).
		Return(map[string]core.UsageStat{"b1": {APIRequests: 10}}, nil)
	store.EXPECT().ListDirectoryChildren(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&core.DirectoryListResult{}, nil)

	da := New(store, stubUsage(ctrl), []string{"b1"}, nil, nil)
	data, err := da.GetData(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if data.QuotaStats["b1"].BytesUsed != 100 {
		t.Errorf("BytesUsed = %d, want 100", data.QuotaStats["b1"].BytesUsed)
	}
	if data.ObjectCounts["b1"] != 5 {
		t.Errorf("ObjectCounts = %d, want 5", data.ObjectCounts["b1"])
	}
	if len(data.BackendOrder) != 1 || data.BackendOrder[0] != "b1" {
		t.Errorf("BackendOrder = %v, want [b1]", data.BackendOrder)
	}
}

// dashboardRead is one of the store reads GetData makes, in issue order, with
// a stub for its success and failure forms.
type dashboardRead struct {
	name string
	ok   func(*storetest.MockDashboardStore)
	fail func(*storetest.MockDashboardStore, error)
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// dashboardReads lists every read GetData issues, in the order it issues them.
func dashboardReads() []dashboardRead {
	a := gomock.Any()
	return []dashboardRead{
		{"quota stats",
			func(m *storetest.MockDashboardStore) {
				m.EXPECT().GetQuotaStats(a).Return(map[string]core.QuotaStat{}, nil)
			},
			func(m *storetest.MockDashboardStore, err error) {
				m.EXPECT().GetQuotaStats(a).Return(nil, err)
			}},
		{"object counts",
			func(m *storetest.MockDashboardStore) {
				m.EXPECT().GetObjectCounts(a).Return(map[string]int64{}, nil)
			},
			func(m *storetest.MockDashboardStore, err error) {
				m.EXPECT().GetObjectCounts(a).Return(nil, err)
			}},
		{"unverified counts",
			func(m *storetest.MockDashboardStore) {
				m.EXPECT().GetUnverifiedObjectCounts(a).Return(map[string]int64{}, nil)
			},
			func(m *storetest.MockDashboardStore, err error) {
				m.EXPECT().GetUnverifiedObjectCounts(a).Return(nil, err)
			}},
		{"integrity coverage",
			func(m *storetest.MockDashboardStore) {
				m.EXPECT().OldestUnverifiedAge(a).Return(time.Duration(0), int64(0), nil)
			},
			func(m *storetest.MockDashboardStore, err error) {
				m.EXPECT().OldestUnverifiedAge(a).Return(time.Duration(0), int64(0), err)
			}},
		{"plaintext copies",
			func(m *storetest.MockDashboardStore) {
				m.EXPECT().CountUnencryptedLocations(a).Return(int64(0), nil)
			},
			func(m *storetest.MockDashboardStore, err error) {
				m.EXPECT().CountUnencryptedLocations(a).Return(int64(0), err)
			}},
		{"compression stats",
			func(m *storetest.MockDashboardStore) {
				m.EXPECT().CompressionStats(a).Return(map[string]core.CompressionStat{}, nil)
			},
			func(m *storetest.MockDashboardStore, err error) {
				m.EXPECT().CompressionStats(a).Return(nil, err)
			}},
		{"multipart counts",
			func(m *storetest.MockDashboardStore) {
				m.EXPECT().GetActiveMultipartCounts(a).Return(map[string]int64{}, nil)
			},
			func(m *storetest.MockDashboardStore, err error) {
				m.EXPECT().GetActiveMultipartCounts(a).Return(nil, err)
			}},
		{"usage stats",
			func(m *storetest.MockDashboardStore) {
				m.EXPECT().GetUsageForPeriod(a, a).Return(map[string]core.UsageStat{}, nil)
			},
			func(m *storetest.MockDashboardStore, err error) {
				m.EXPECT().GetUsageForPeriod(a, a).Return(nil, err)
			}},
		{"directory children",
			func(m *storetest.MockDashboardStore) {
				m.EXPECT().ListDirectoryChildren(a, a, a, a).Return(&core.DirectoryListResult{}, nil)
			},
			func(m *storetest.MockDashboardStore, err error) {
				m.EXPECT().ListDirectoryChildren(a, a, a, a).Return(nil, err)
			}},
	}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestAggregator_ReadErrorsFailGetData asserts every store read is
// load-bearing: whichever one fails, GetData fails rather than returning a
// partially-populated dashboard. Each case stubs the reads issued before the
// failing one so the aggregator actually reaches it.
func TestAggregator_ReadErrorsFailGetData(t *testing.T) {
	t.Parallel()

	reads := dashboardReads()
	for i, c := range reads {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctrl := gomock.NewController(t)
			store := storetest.NewMockDashboardStore(ctrl)
			for _, earlier := range reads[:i] {
				earlier.ok(store)
			}
			c.fail(store, errors.New("db down"))

			da := New(store, stubUsage(ctrl), nil, nil, nil)
			if _, err := da.GetData(t.Context()); err == nil {
				t.Fatalf("GetData succeeded despite %s failing", c.name)
			}
		})
	}
}

// TestAggregator_GetDirectoryChildren_ClampsMaxKeys pins the bound the
// aggregator puts on a caller-supplied page size. The assertion is the
// argument the store receives, not just that a result came back.
func TestAggregator_GetDirectoryChildren_ClampsMaxKeys(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name    string
		maxKeys int
	}{
		{"unset", 0},
		{"negative", -1},
		{"over the clamp", 999},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctrl := gomock.NewController(t)
			store := storetest.NewMockDashboardStore(ctrl)
			store.EXPECT().ListDirectoryChildren(gomock.Any(), gomock.Any(), gomock.Any(), maxDirectoryChildren).
				Return(&core.DirectoryListResult{}, nil).Times(1)

			da := New(store, stubUsage(ctrl), nil, nil, nil)
			if _, err := da.GetDirectoryChildren(t.Context(), "", "", c.maxKeys); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestAggregator_GetDirectoryChildren_PassesThroughInRange asserts a page size
// inside the bound reaches the store unchanged.
func TestAggregator_GetDirectoryChildren_PassesThroughInRange(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockDashboardStore(ctrl)
	store.EXPECT().ListDirectoryChildren(gomock.Any(), "photos/", "cat.jpg", 25).
		Return(&core.DirectoryListResult{}, nil).Times(1)

	da := New(store, stubUsage(ctrl), nil, nil, nil)
	if _, err := da.GetDirectoryChildren(t.Context(), "photos/", "cat.jpg", 25); err != nil {
		t.Fatal(err)
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
			d := &Data{CompressionStats: tt.stats}

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
	d := &Data{CompressionStats: map[string]core.CompressionStat{
		"b1": {Objects: 1, LogicalBytes: 900, StoredBytes: 100},
	}}
	if got := d.CompressionSaved("b2"); got != 0 {
		t.Errorf("CompressionSaved(b2) = %d, want 0", got)
	}
}
