// -------------------------------------------------------------------------------
// CLI Output - Backend Name List and Usage Limit Tests
//
// Author: Alex Freidah
//
// Covers the comma join and the dash for an empty name list, the instance
// line, and how a usage limit is named and in which units its figures render.
// -------------------------------------------------------------------------------

package output

import (
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// TestNameList covers the comma join and the dash for an empty list, nil or
// not.
func TestNameList(t *testing.T) {
	t.Parallel()
	if got := NameList([]string{"cordoned", "draining"}); got != "cordoned,draining" {
		t.Errorf("NameList = %q, want cordoned,draining", got)
	}
	if NameList(nil) != "-" || NameList([]string{}) != "-" {
		t.Error("NameList of an empty list should be a dash")
	}
}

// TestMeterLabel verifies a byte limit is named by its kind and a request
// budget by kind and budget.
func TestMeterLabel(t *testing.T) {
	t.Parallel()
	if got := MeterLabel(core.MeterEgress, ""); got != "egress" {
		t.Errorf("MeterLabel(egress) = %q", got)
	}
	if got := MeterLabel(core.MeterRequests, "writes"); got != "requests:writes" {
		t.Errorf("MeterLabel(requests, writes) = %q", got)
	}
}

// TestInstance verifies the instance line names the id, build, mode and
// uptime, and that a server reporting no identity reads as unknown.
func TestInstance(t *testing.T) {
	t.Parallel()
	in := adminapi.InstanceStatus{ID: "host-1a2b3c4d", Version: "v1.2.3", Mode: "all", StartedAt: time.Now().Add(-3 * time.Hour)}
	if got, want := Instance(in), "host-1a2b3c4d  v1.2.3  mode all  up 3h"; got != want {
		t.Errorf("Instance = %q, want %q", got, want)
	}
	if got := Instance(adminapi.InstanceStatus{}); got != "unknown" {
		t.Errorf("Instance of an empty identity = %q, want unknown", got)
	}
}

// TestMeterFormat verifies byte limits render in IEC units and request
// budgets as a call count.
func TestMeterFormat(t *testing.T) {
	t.Parallel()
	if got := MeterFormat(core.MeterIngress)(2048); got != "2.0 KiB" {
		t.Errorf("ingress figure = %q, want 2.0 KiB", got)
	}
	if got := MeterFormat(core.MeterRequests)(2048); got != "2,048" {
		t.Errorf("requests figure = %q, want 2,048", got)
	}
}
