// -------------------------------------------------------------------------------
// UI Handler - Authorization
//
// Author: Alex Freidah
//
// Refuses dashboard requests the signed-in user's grants do not carry, against
// the same permissions the admin API checks for the same operations. A route
// acting on the fleet or its backends is authorized from its table entry before
// the handler runs; a route acting on objects names its bucket in the request
// body or query, so its handler authorizes the key once it has read it.
// -------------------------------------------------------------------------------

package ui

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// msgForbidden is the whole refusal body. It says nothing about what would have
// been allowed, so a response cannot be used to map the grants a user holds.
const msgForbidden = "forbidden"

// dashboardAccess is what signing in to the dashboard requires: the overview
// every page renders is fleet state, which admin-read covers.
var dashboardAccess = core.Resource{Kind: core.ResourceOrchestrator}

// userKey carries the signed-in user from requireAuth to the handlers.
type userKey struct{}

// withUser returns ctx carrying u.
func withUser(ctx context.Context, u *auth.User) context.Context {
	return context.WithValue(ctx, userKey{}, u)
}

// userFrom returns the signed-in user requireAuth attached, or nil, which every
// grant check refuses.
func userFrom(ctx context.Context) *auth.User {
	u, _ := ctx.Value(userKey{}).(*auth.User)
	return u
}

// canUseDashboard reports whether u may sign in and see the overview.
func canUseDashboard(u *auth.User) bool {
	return u.CanAdmin(dashboardAccess, core.PermAdminRead)
}

// authorizeRoute refuses a control-plane route the user's grants do not carry.
// Reports whether the request may proceed; the refusal is already written when
// it may not. Bucket routes pass here and are authorized by their handler.
// Backend operations run over every backend, so they require the backend
// wildcard grant.
func (h *Handler) authorizeRoute(w http.ResponseWriter, r *http.Request, rt *uiAPIRoute) bool {
	if rt.perm == 0 {
		h.refuse(w, r, rt.suffix, "route declares no permission", "")
		return false
	}
	if rt.kind == core.ResourceBucket {
		return true
	}
	resource := core.Resource{Kind: rt.kind}
	if rt.kind == core.ResourceBackend {
		resource.Name = core.ResourceWildcard
	}
	if !userFrom(r.Context()).CanAdmin(resource, rt.perm) {
		h.refuse(w, r, rt.suffix, "grant does not carry the permission", resource.String())
		return false
	}
	return true
}

// authorizeKey refuses a request whose key names a bucket the user may not
// exercise want on. Reports whether it may proceed.
func (h *Handler) authorizeKey(w http.ResponseWriter, r *http.Request, key string, want core.PermissionSet) bool {
	bucket, reason, ok := auth.AuthorizeKey(userFrom(r.Context()), key, want)
	if !ok {
		h.refuse(w, r, strings.TrimPrefix(r.URL.Path, h.prefix), reason, bucket)
	}
	return ok
}

// canList reports whether the signed-in user may list the named bucket, for the
// views that enumerate buckets rather than act on one.
func canList(ctx context.Context, bucket string) bool {
	return userFrom(ctx).Can(bucket, core.PermList)
}

// refuse writes the 403 and records it as ui.ActionDenied, named to match the
// admin API's admin.ActionDenied so one query finds a refusal on either surface.
func (h *Handler) refuse(w http.ResponseWriter, r *http.Request, route, reason, resource string) {
	user := "unknown"
	if u := userFrom(r.Context()); u != nil {
		user = u.ID
	}
	h.log.WarnContext(r.Context(), "dashboard request not permitted by grant",
		"path", r.URL.Path,
		"client_addr", r.RemoteAddr,
		"user", user,
		"resource", resource,
		"reason", reason,
	)
	audit.Log(r.Context(), "ui.ActionDenied",
		slog.String("route", route),
		slog.String("client_addr", r.RemoteAddr),
		slog.String("resource", resource),
		slog.String("reason", reason),
		slog.Int("status", http.StatusForbidden),
	)
	httputil.WriteJSONError(w, http.StatusForbidden, msgForbidden)
}
