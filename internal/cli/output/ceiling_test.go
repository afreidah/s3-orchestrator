// -------------------------------------------------------------------------------
// CLI Output - Ceiling Tests
//
// Author: Alex Freidah
//
// Covers how a storage quota and a usage limit become ceilings, the share and
// percentage of a ceiling with and without a limit, which ceiling across a
// fleet is nearest full, and how long until it resets.
// -------------------------------------------------------------------------------

package output

import (
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// TestUsageCeiling verifies a byte limit is named by its kind and renders in
// IEC units, and a request budget is named by kind and budget and counts calls.
func TestUsageCeiling(t *testing.T) {
	t.Parallel()
	egress := UsageCeiling(adminapi.UsageMeter{Kind: core.MeterEgress, Used: 1024, Limit: 2048})
	if egress.Label != "egress" || egress.Figures() != "1.0 KiB / 2.0 KiB" {
		t.Errorf("egress ceiling = %q %q", egress.Label, egress.Figures())
	}
	writes := UsageCeiling(adminapi.UsageMeter{Kind: core.MeterRequests, Budget: "writes", Used: 1200, Limit: 4800})
	if writes.Label != "requests:writes" || writes.Figures() != "1,200 / 4,800" {
		t.Errorf("request budget ceiling = %q %q", writes.Label, writes.Figures())
	}
}

// TestCeiling_Shares verifies the share, percentage and its text for a
// ceiling with a limit, and that a ceiling with none reads as unused.
func TestCeiling_Shares(t *testing.T) {
	t.Parallel()
	c := StorageCeiling(&adminapi.BackendStatus{BytesUsed: 3, BytesLimit: 4})
	if c.Label != StorageLabel || c.Share() != 0.75 || c.Percent() != 75 || c.PercentText() != "75%" {
		t.Errorf("storage ceiling = %q %v %d %q", c.Label, c.Share(), c.Percent(), c.PercentText())
	}
	none := StorageCeiling(&adminapi.BackendStatus{BytesUsed: 3})
	if none.Share() != 0 || none.Percent() != 0 || none.PercentText() != "-" {
		t.Errorf("ceiling with no limit = %v %d %q", none.Share(), none.Percent(), none.PercentText())
	}
}

// TestNearestLimit verifies the fullest ceiling wins across storage quotas and
// usage limits by its exact share, carries its backend's next reset, and that
// a storage quota carries none.
func TestNearestLimit(t *testing.T) {
	t.Parallel()
	reset := time.Now().Add(4*24*time.Hour + time.Hour)
	backends := []adminapi.BackendStatus{
		{Name: "a", BytesUsed: 870, BytesLimit: 1000},
		{Name: "b", BytesUsed: 1, BytesLimit: 1000,
			UsageLimits: []adminapi.UsageMeter{
				{Kind: core.MeterEgress, Used: 1, Limit: 10},
				{Kind: core.MeterRequests, Budget: "all", Used: 874, Limit: 1000},
			},
			UsageCycle: adminapi.UsageCycle{NextReset: reset}},
	}
	n, ok := NearestLimit(backends)
	if !ok || n.Backend != "b" || n.Label != "requests:all" || !n.Reset.Equal(reset) {
		t.Fatalf("NearestLimit = %+v, %t; want b requests:all", n, ok)
	}
	if got := n.ResetText(); got != "resets in 4d" {
		t.Errorf("ResetText = %q, want resets in 4d", got)
	}

	backends[0].BytesUsed = 999
	n, _ = NearestLimit(backends)
	if n.Backend != "a" || n.Label != StorageLabel || n.ResetText() != "" {
		t.Errorf("NearestLimit = %+v, want a's storage quota with no reset", n)
	}
}

// TestNearestLimit_NoLimits verifies a fleet with no limit anywhere has no
// nearest one.
func TestNearestLimit_NoLimits(t *testing.T) {
	t.Parallel()
	if n, ok := NearestLimit([]adminapi.BackendStatus{{Name: "a", BytesUsed: 5}}); ok {
		t.Errorf("NearestLimit = %+v, want none", n)
	}
	if _, ok := NearestLimit(nil); ok {
		t.Error("NearestLimit of no backends found one")
	}
}

// TestCycle verifies a cycle renders as its days and zone, and a cycle a
// server did not report as a dash.
func TestCycle(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	c := adminapi.UsageCycle{Start: start, NextReset: start.AddDate(0, 1, 0), Timezone: "UTC"}
	if got := Cycle(c); got != "Oct 15 - Nov 15 UTC" {
		t.Errorf("Cycle = %q", got)
	}
	if got := Cycle(adminapi.UsageCycle{}); got != "-" {
		t.Errorf("Cycle of no cycle = %q, want -", got)
	}
}
