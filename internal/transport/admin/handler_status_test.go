// -------------------------------------------------------------------------------
// Admin API - Status Mapping Tests
//
// Author: Alex Freidah
//
// Unit tests for backendStatuses, the pure dashboard-to-wire mapping behind
// /admin/api/status: health inversion, drain-by-presence, per-backend counters,
// usage limits, and display ordering.
// -------------------------------------------------------------------------------

package admin

import (
	"slices"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/proxy/backendstate"
	"github.com/afreidah/s3-orchestrator/internal/proxy/dashboard"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// TestBackendStatuses_Mapping covers the health/drain/counter mapping and that
// rows follow BackendOrder.
func TestBackendStatuses_Mapping(t *testing.T) {
	t.Parallel()
	data := &dashboard.Data{
		BackendOrder:     []string{"b1", "b2"},
		RefusedCalls:     map[string]core.BackendCalls{"b2": core.CallGetObject | core.CallHeadObject},
		DrainingBackends: map[string]backendstate.DrainProgress{"b1": {Active: true, State: "draining"}},
		BackendFlags:     map[string]core.BackendFlags{"b1": core.FlagCordoned | core.FlagDraining},
		QuotaStats: map[string]core.QuotaStat{
			"b1": {BytesUsed: 100, BytesLimit: 1000},
		},
		ObjectCounts: map[string]int64{"b1": 7},
		UsageStats: map[string]core.Usage{
			"b1": {UsageStat: core.UsageStat{APIRequests: 3, EgressBytes: 20, IngressBytes: 40}},
		},
	}

	got := backendStatuses(data)
	if len(got) != 2 || got[0].Name != "b1" || got[1].Name != "b2" {
		t.Fatalf("rows out of order: %+v", got)
	}

	// b1: healthy (absent from refused calls), cordoned and draining, counters mapped.
	b1 := got[0]
	if !b1.Healthy || !b1.Draining || !slices.Equal(b1.Flags, []string{"cordoned", "draining"}) {
		t.Errorf("b1 health/drain/flags = %v/%v/%v, want true/true/[cordoned draining]", b1.Healthy, b1.Draining, b1.Flags)
	}
	if b1.BytesUsed != 100 || b1.BytesLimit != 1000 || b1.ObjectCount != 7 {
		t.Errorf("b1 quota/count = %+v", b1)
	}
	if b1.APIRequests != 3 || b1.EgressBytes != 20 || b1.IngressBytes != 40 {
		t.Errorf("b1 usage = %+v", b1)
	}

	// b2: refusing GET and HEAD, not draining, and no stats seeded => zero counters.
	b2 := got[1]
	if b2.Healthy || b2.Draining {
		t.Errorf("b2 health/drain = %v/%v, want false/false", b2.Healthy, b2.Draining)
	}
	// A backend in service reports an empty list, which renders as [] rather
	// than being omitted.
	if b2.Flags == nil || len(b2.Flags) != 0 {
		t.Errorf("b2 flags = %#v, want an empty non-nil list", b2.Flags)
	}
	if b2.BytesUsed != 0 || b2.ObjectCount != 0 || b2.APIRequests != 0 {
		t.Errorf("b2 should have zero counters: %+v", b2)
	}
}

// TestBackendStatuses_UsageLimits verifies each backend carries a meter per
// limit it enforces, measured against its usage, and an unlimited backend an
// empty list rather than none.
func TestBackendStatuses_UsageLimits(t *testing.T) {
	t.Parallel()
	lim, err := core.NewUsageLimits(1000, 500, core.SingleRequestPool(100), nil)
	if err != nil {
		t.Fatalf("NewUsageLimits: %v", err)
	}
	got := backendStatuses(&dashboard.Data{
		BackendOrder: []string{"b1", "b2"},
		UsageLimits:  map[string]core.UsageLimits{"b1": lim},
		UsageStats: map[string]core.Usage{
			"b1": {UsageStat: core.UsageStat{EgressBytes: 20, IngressBytes: 40}, Pools: core.PoolUsage{core.PoolAll: 3}},
		},
	})

	want := []adminapi.UsageMeter{
		{Kind: core.MeterEgress, Used: 20, Limit: 1000},
		{Kind: core.MeterIngress, Used: 40, Limit: 500},
		{Kind: core.MeterRequests, Budget: core.PoolAll, Used: 3, Limit: 100},
	}
	if !slices.Equal(got[0].UsageLimits, want) {
		t.Errorf("b1 usage limits = %+v, want %+v", got[0].UsageLimits, want)
	}
	if got[1].UsageLimits == nil || len(got[1].UsageLimits) != 0 {
		t.Errorf("b2 usage limits = %#v, want an empty non-nil list", got[1].UsageLimits)
	}
}

// TestBackendStatuses_UsageCycle verifies each backend carries its own cycle,
// with its bounds and the IANA name of the zone it is counted in.
func TestBackendStatuses_UsageCycle(t *testing.T) {
	t.Parallel()
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	mid := core.UsagePeriod{ResetDay: 15, Location: la}.Cycle(now)
	cal := core.UsagePeriod{}.Cycle(now)
	got := backendStatuses(&dashboard.Data{
		BackendOrder: []string{"b1", "b2"},
		UsageCycles:  map[string]core.UsageCycle{"b1": mid, "b2": cal},
	})

	if c := got[0].UsageCycle; !c.Start.Equal(mid.Start) || !c.NextReset.Equal(mid.Next) || c.Timezone != "America/Los_Angeles" {
		t.Errorf("b1 cycle = %+v, want %v - %v in America/Los_Angeles", c, mid.Start, mid.Next)
	}
	if c := got[1].UsageCycle; !c.Start.Equal(cal.Start) || !c.NextReset.Equal(cal.Next) || c.Timezone != "UTC" {
		t.Errorf("b2 cycle = %+v, want the calendar month in UTC", c)
	}
}

// TestBackendStatuses_RefusedCalls covers the refused-call names: listed in
// call order for a backend refusing some, and an empty list for one serving all.
func TestBackendStatuses_RefusedCalls(t *testing.T) {
	t.Parallel()
	got := backendStatuses(&dashboard.Data{
		BackendOrder: []string{"b1", "b2"},
		RefusedCalls: map[string]core.BackendCalls{"b2": core.CallGetObject | core.CallHeadObject},
	})

	if got[0].RefusedCalls == nil || len(got[0].RefusedCalls) != 0 {
		t.Errorf("b1 refused calls = %#v, want an empty non-nil list", got[0].RefusedCalls)
	}
	if want := []string{"GetObject", "HeadObject"}; !slices.Equal(got[1].RefusedCalls, want) {
		t.Errorf("b2 refused calls = %v, want %v", got[1].RefusedCalls, want)
	}
}
