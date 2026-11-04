// -------------------------------------------------------------------------------
// CLI Output - Backend Name List Tests
//
// Author: Alex Freidah
//
// Covers the comma join and the dash for an empty name list, and the instance
// line.
// -------------------------------------------------------------------------------

package output

import (
	"testing"
	"time"

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

// TestBackendCellsOf verifies the cells read a quota as figures and a share, and
// no quota as no limit rather than a limit of zero.
func TestBackendCellsOf(t *testing.T) {
	t.Parallel()
	limited := BackendCellsOf(&adminapi.BackendStatus{BytesUsed: 2048, BytesLimit: 4096, ObjectCount: 3, Flags: []string{"cordoned"}})
	want := BackendCells{Health: "healthy", Refused: "-", Flags: "cordoned", Used: "2.0 KiB", Limit: "4.0 KiB", Use: "50%", Objects: "3"}
	if limited != want {
		t.Errorf("limited cells = %+v, want %+v", limited, want)
	}
	if got := BackendCellsOf(&adminapi.BackendStatus{}); got.Limit != NoLimit || got.Use != "-" {
		t.Errorf("unlimited cells = %+v, want no limit and a dash", got)
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
