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

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

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
