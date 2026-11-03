// -------------------------------------------------------------------------------
// Auth - Refused Request Record
//
// Author: Alex Freidah
//
// What the admin API and the dashboard record when a user's grants do not
// carry a request: one warning and one audit entry, naming what was asked of
// which resource, what that required against what the user held, and why. Both
// surfaces record through here, so an operator reading either can fix the
// grant without a second lookup.
// -------------------------------------------------------------------------------

package auth

import (
	"log/slog"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// Reasons a request is refused besides the ones AuthorizeKey reports.
const (
	ReasonNoRoutePermission = "route declares no permission"
)

// Denial is one refused request: the audit event it is filed under, the user
// refused (nil when none signed in), the operation asked for, the resource it
// was asked of, the permission that required, and why it was refused.
type Denial struct {
	Event     string
	User      *User
	Operation string
	Resource  core.Resource
	Required  core.PermissionSet
	Reason    string
}

// Record logs the denial as a warning and writes its audit entry, both naming
// the request's method, path and client address.
func (d *Denial) Record(r *http.Request, log *slog.Logger) {
	attrs := []slog.Attr{
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.String("client_addr", r.RemoteAddr),
		slog.String("operation", d.Operation),
		slog.String("resource", d.Resource.String()),
		slog.String("required", d.Required.String()),
		slog.String("held", d.held().String()),
		slog.String("reason", d.Reason),
	}
	log.LogAttrs(r.Context(), slog.LevelWarn, "request not permitted by grant",
		append(attrs, slog.String("event", d.Event), slog.String("user", d.userID()))...)
	audit.Log(r.Context(), d.Event, append(attrs, slog.Int("status", http.StatusForbidden))...)
}

// held is what the user's grants carry on the resource: the bucket grant for
// a bucket, the control-plane grant otherwise.
func (d *Denial) held() core.PermissionSet {
	if d.Resource.Kind == core.ResourceBucket {
		held, _ := d.User.Permissions(d.Resource.Name)
		return held
	}
	return d.User.AdminPermissions(d.Resource)
}

// userID names the refused user, or "unknown" when none signed in.
func (d *Denial) userID() string {
	if d.User == nil {
		return "unknown"
	}
	return d.User.ID
}
