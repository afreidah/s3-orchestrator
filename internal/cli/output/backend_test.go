// -------------------------------------------------------------------------------
// CLI Output - Backend Name List and Usage Limit Tests
//
// Author: Alex Freidah
//
// Covers the comma join and the dash for an empty name list, and how a usage
// limit is named and in which units its figures render.
// -------------------------------------------------------------------------------

package output

import (
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
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
