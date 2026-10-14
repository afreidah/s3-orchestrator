// -------------------------------------------------------------------------------
// CLI Output - Backend Health and Name List Tests
//
// Author: Alex Freidah
//
// Covers the three health words and the dash for an empty name list.
// -------------------------------------------------------------------------------

package output

import (
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// TestBackendHealth covers healthy for nothing refused, degraded for some
// calls and down for every call.
func TestBackendHealth(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		refused []string
		want    string
	}{
		{nil, HealthHealthy},
		{[]string{"GetObject"}, HealthDegraded},
		{core.AllCalls.Names(), HealthDown},
	} {
		if got := BackendHealth(tc.refused); got != tc.want {
			t.Errorf("BackendHealth(%v) = %q, want %q", tc.refused, got, tc.want)
		}
	}
}

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
