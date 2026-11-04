// -------------------------------------------------------------------------------
// CLI Output - Grant Tests
//
// Author: Alex Freidah
//
// Pins the complete-set shorthand, which keeps a listing's grants column
// inside a terminal: the full control-plane set is ten names.
// -------------------------------------------------------------------------------

package output

import (
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// TestPermissions verifies a complete set renders as the one word that names
// it and anything less lists every name.
func TestPermissions(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		perms []string
		want  string
	}{
		{"the full data-plane set", core.PermAll.Names(), "(all)"},
		{"the full admin set", core.PermAdminAll.Names(), "(admin-all)"},
		{"a partial set is listed", []string{"read", "write"}, "(read,write)"},
		{"one permission", []string{"admin-read"}, "(admin-read)"},
		{"nothing", nil, "()"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Permissions(tc.perms); got != tc.want {
				t.Errorf("Permissions(%v) = %q, want %q", tc.perms, got, tc.want)
			}
		})
	}
}
