// -------------------------------------------------------------------------------
// CLI Output - Grants
//
// Author: Alex Freidah
//
// Renders what a grant carries the same way in admin's user listing and the
// TUI's buckets pane, collapsing a complete permission set to the one word
// the stored form names it by.
// -------------------------------------------------------------------------------

package output

import (
	"slices"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// Permissions renders a grant's permissions in parentheses, as "(read,write)",
// with a complete set as the one word that names it, as "(all)". The API
// returns every name; only the listings abbreviate.
func Permissions(perms []string) string {
	return "(" + permissionList(perms) + ")"
}

// permissionList joins the names, or names the complete set they make.
func permissionList(perms []string) string {
	for _, full := range []core.PermissionSet{core.PermAll, core.PermAdminAll} {
		if slices.Equal(perms, full.Names()) {
			return full.String()
		}
	}
	return strings.Join(perms, ",")
}
