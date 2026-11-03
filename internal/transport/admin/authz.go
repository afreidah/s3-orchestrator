// -------------------------------------------------------------------------------
// Admin API - Authentication and Authorization
//
// Author: Alex Freidah
//
// Resolves the identity behind an admin request and refuses the ones its grants
// do not reach. The data-plane endpoints under /admin/api/objects reach the same
// object service the S3 transport does, so they are authorized against the same
// bucket permission set.
//
// Every route passes through one chokepoint here. The control-plane endpoints
// declare a permission over a backend or over the instance, so a credential
// granted one provider's maintenance cannot start a pass over the fleet.
// -------------------------------------------------------------------------------

package admin

import (
	"cmp"
	"log/slog"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// The refusal messages. A caller is told it was refused and nothing about what
// would have been allowed, so the response cannot be used to map the grants an
// identity holds.
const (
	msgUnauthorized = "unauthorized"
	msgForbidden    = "forbidden"
)

// -------------------------------------------------------------------------
// AUTHENTICATION
// -------------------------------------------------------------------------

// authenticate resolves the credential a request carries to the user it proves,
// and reports whether it proved one at all. Every caller is a user holding
// grants, the root credential included: what makes root privileged is the
// grants it holds, not a branch here.
//
// A SigV4-signed request is verified the way the S3 API verifies one, against
// the same registry, so one keypair reaches both surfaces.
func (h *Handler) authenticate(r *http.Request) (*auth.User, bool) {
	// A worker-mode instance has no registry, so nothing is authenticated.
	registry := h.d.Registry()
	if registry == nil {
		return nil, false
	}
	user, _, err := registry.Authenticate(r)
	if err != nil {
		return nil, false
	}
	return user, true
}

// -------------------------------------------------------------------------
// AUTHORIZATION
// -------------------------------------------------------------------------

// guard wraps one route with the authentication and authorization its table
// entry declares, and is the only path a request reaches a handler by. Every
// response, a refusal included, names the instance that answered.
func (h *Handler) guard(rt *route) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(adminapi.HeaderInstance, h.d.Instance.ID)
		who, ok := h.authenticate(r)
		if !ok {
			h.log.LogAttrs(r.Context(), slog.LevelWarn, "unauthorized request", logfmt.Request(r)...)
			httputil.WriteJSONError(w, http.StatusUnauthorized, msgUnauthorized)
			return
		}
		if !h.authorize(w, r, rt, who) {
			return
		}
		rt.Handler(w, r.WithContext(audit.WithWorkload(r.Context(), audit.WorkloadAdmin)))
	}
}

// authorize refuses a request whose grants do not carry what its route needs.
// Reports whether it may proceed; the refusal is already written when it may not.
//
// Fails closed: a route declaring no permission is refused, and so is a bucket
// route whose resource does not resolve to one bucket.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, rt *route, who *auth.User) bool {
	// Nothing reaches a route that declared no permission. The table is tested
	// for one, so this is unreachable by a served route and stays as the
	// fallback that keeps the default closed.
	if rt.Perm == 0 {
		h.refuse(w, r, &auth.Denial{
			Event: eventActionDenied, User: who, Operation: rt.Summary,
			Resource: core.Resource{Kind: rt.kind()}, Reason: auth.ReasonNoRoutePermission,
		})
		return false
	}
	if rt.kind() != core.ResourceBucket {
		return h.authorizeAdmin(w, r, rt, who)
	}

	bucket, reason, ok := auth.AuthorizeKey(who, h.resourceValue(r, rt), rt.Perm)
	if !ok {
		h.refuse(w, r, &auth.Denial{
			Event: eventActionDenied, User: who, Operation: rt.Summary,
			Resource: core.Resource{Kind: core.ResourceBucket, Name: bucket}, Required: rt.Perm, Reason: reason,
		})
		return false
	}
	return true
}

// authorizeAdmin refuses a control-plane request the caller's grants do not
// carry. A backend route naming no backend runs fleet-wide, so it requires the
// wildcard grant; a grant on one backend cannot start a pass over the others.
func (h *Handler) authorizeAdmin(w http.ResponseWriter, r *http.Request, rt *route, who *auth.User) bool {
	resource := core.Resource{Kind: rt.kind()}
	if resource.Kind == core.ResourceBackend {
		resource.Name = cmp.Or(h.resourceValue(r, rt), core.ResourceWildcard)
	}
	if !who.CanAdmin(resource, rt.Perm) {
		h.refuse(w, r, &auth.Denial{
			Event: eventControlPlaneDenied, User: who, Operation: rt.Summary,
			Resource: resource, Required: rt.Perm, Reason: auth.ReasonPermission,
		})
		return false
	}
	return true
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// resourceValue reads the parameter naming the object key. A path value is
// empty for a parameter the pattern does not declare, so one lookup covers both
// the routes carrying the key in the path and those carrying it in the query.
func (h *Handler) resourceValue(r *http.Request, rt *route) string {
	return cmp.Or(r.PathValue(rt.Resource), r.URL.Query().Get(rt.Resource))
}

// backendParam reads the optional backend a pass is restricted to, and reports
// whether the request may proceed. An empty value runs against every backend.
// An unknown name is refused, since a typo would otherwise report a clean pass
// over nothing.
func (h *Handler) backendParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.URL.Query().Get(paramBackend)
	if name == "" {
		return "", true
	}
	if !h.d.Config.Load().HasBackend(name) {
		httputil.WriteJSONError(w, http.StatusBadRequest, "unknown backend: "+name)
		return "", false
	}
	return name, true
}

// The audit events a refusal is filed under. eventActionDenied matches the
// s3.ActionDenied entry the data path emits, so one query finds an object
// refusal whichever surface it happened on; control-plane refusals are filed
// apart so one query finds them without the object traffic.
const (
	eventActionDenied       = "admin.ActionDenied"
	eventControlPlaneDenied = "admin.ControlPlaneDenied"
)

// refuse records d and writes the 403.
func (h *Handler) refuse(w http.ResponseWriter, r *http.Request, d *auth.Denial) {
	d.Record(r, h.log)
	httputil.WriteJSONError(w, http.StatusForbidden, msgForbidden)
}
