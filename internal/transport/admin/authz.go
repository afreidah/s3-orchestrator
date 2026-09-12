// -------------------------------------------------------------------------------
// Admin API - Authentication and Authorization
//
// Author: Alex Freidah
//
// Resolves the identity behind an admin request and refuses the ones its grants
// do not reach. The data-plane endpoints under /admin/api/objects reach the same
// object service the S3 transport does, so they are authorized against the same
// permission set rather than against the bearer token alone.
//
// Every route passes through one chokepoint here. A route that declares no
// permission is control plane and the token authorizes it, unchanged.
// -------------------------------------------------------------------------------

package admin

import (
	"cmp"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// adminTokenHeader carries the credential proving an admin request.
const adminTokenHeader = "X-Admin-Token"

// The refusal messages. A caller is told it was refused and nothing about what
// would have been allowed, so the response cannot be used to map the grants an
// identity holds.
const (
	msgUnauthorized = "unauthorized"
	msgForbidden    = "forbidden"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// principal is who an admin request authenticated as.
//
// Root marks the deprecated fallback: the configured admin token authorizes
// everything, which is what an existing deployment relies on. It is deliberately
// not a property of auth.User - the S3 path shares that type and has no such
// caller - so removing the fallback removes this field with it rather than
// leaving a privileged flag behind in the identity model.
type principal struct {
	User *auth.User
	Root bool
}

// -------------------------------------------------------------------------
// AUTHENTICATION
// -------------------------------------------------------------------------

// authenticate resolves the credential a request carries. Reports whether one
// proved an identity at all.
//
// The configured admin token is tried first and answers as root. Anything else
// is looked up as a provisioned credential, so a token minted through the
// provisioning API reaches the admin API with exactly the grants it was given.
func (h *Handler) authenticate(r *http.Request) (principal, bool) {
	token := r.Header.Get(adminTokenHeader)
	if token == "" {
		return principal{}, false
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(h.token)) == 1 {
		return principal{Root: true}, true
	}

	registry := h.registry()
	if registry == nil {
		return principal{}, false
	}
	user, err := registry.AuthenticateToken(token)
	if err != nil {
		return principal{}, false
	}
	return principal{User: user}, true
}

// -------------------------------------------------------------------------
// AUTHORIZATION
// -------------------------------------------------------------------------

// guard wraps one route with the authentication and authorization its table
// entry declares, and is the only path a request reaches a handler by.
func (h *Handler) guard(rt *route) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		who, ok := h.authenticate(r)
		if !ok {
			h.log.WarnContext(r.Context(), "unauthorized request",
				"path", r.URL.Path, "client_addr", r.RemoteAddr)
			httputil.WriteJSONError(w, http.StatusUnauthorized, msgUnauthorized)
			return
		}
		if !h.authorize(w, r, rt, who) {
			return
		}
		rt.Handler(w, r)
	}
}

// authorize refuses a request whose grants do not carry what its route needs.
// Reports whether it may proceed; the refusal is already written when it may not.
//
// Fails closed. A route declaring a permission whose resource does not resolve
// to a bucket is refused rather than allowed through unchecked, because a key
// this layer cannot read is one it cannot authorize.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, rt *route, who principal) bool {
	// The control plane has no action vocabulary yet, so the admin token stays
	// its authorization. A provisioned credential carries bucket grants, which
	// say nothing about draining a backend or rotating a key, and treating an
	// absent permission as "no check" would let any S3 credential reach every
	// fleet operation.
	if rt.Perm == 0 {
		if who.Root {
			return true
		}
		h.refuseControlPlane(r, w, rt, who)
		return false
	}
	if who.Root {
		h.warnTokenDeprecated(r)
		return true
	}

	bucket, ok := bucketFromKey(h.resourceValue(r, rt))
	if !ok {
		h.refuse(r, w, rt, who, "", "resource names no bucket")
		return false
	}
	if !who.User.CanReach(bucket) {
		h.refuse(r, w, rt, who, bucket, "no grant on the bucket")
		return false
	}
	if !who.User.Can(bucket, rt.Perm) {
		h.refuse(r, w, rt, who, bucket, "grant does not carry the permission")
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
//
// An unknown name is refused rather than run: a filter matching nothing is
// indistinguishable from a fleet with no work left, so a typo would report a
// clean pass over a backend that was never read.
func (h *Handler) backendParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.URL.Query().Get(paramBackend)
	if name == "" {
		return "", true
	}
	if !slices.Contains(h.backendNames(), name) {
		httputil.WriteJSONError(w, http.StatusBadRequest, "unknown backend: "+name)
		return "", false
	}
	return name, true
}

// bucketFromKey reads the bucket an admin object key names. The admin API
// carries bucket and key as one string and a bucket name holds no slash, so the
// first segment is the bucket.
//
// A value with no slash names no single bucket: the empty prefix is the whole
// namespace, and a partial name spans every bucket it prefixes. Neither can be
// authorized against one grant, so both report false and are refused.
func bucketFromKey(key string) (string, bool) {
	bucket, _, found := strings.Cut(key, "/")
	if !found || bucket == "" {
		return "", false
	}
	return bucket, true
}

// refuse writes the 403 and records what was asked for against what was held,
// so an operator reading the audit stream can fix the grant without a second
// lookup. Named to match the s3.ActionDenied entry the data path emits, so one
// query finds a refusal whichever surface it happened on.
func (h *Handler) refuse(r *http.Request, w http.ResponseWriter, rt *route, who principal, bucket, reason string) {
	held, _ := who.User.Permissions(bucket)
	h.log.WarnContext(r.Context(), "admin request not permitted by grant",
		"method", r.Method,
		"path", r.URL.Path,
		"client_addr", r.RemoteAddr,
		"user", who.User.ID,
		"bucket", bucket,
		"reason", reason,
	)
	audit.Log(r.Context(), "admin.ActionDenied",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.String("client_addr", r.RemoteAddr),
		slog.String("bucket", bucket),
		slog.String("operation", rt.Summary),
		slog.String("required", rt.Perm.String()),
		slog.String("held", held.String()),
		slog.String("reason", reason),
		slog.Int("status", http.StatusForbidden),
	)
	httputil.WriteJSONError(w, http.StatusForbidden, msgForbidden)
}

// refuseControlPlane writes the 403 a provisioned credential gets when it asks
// for a fleet operation. Recorded separately from a denied object operation
// because the fix differs: one is a grant to widen, the other a caller using the
// wrong credential entirely for the surface it is calling.
func (h *Handler) refuseControlPlane(r *http.Request, w http.ResponseWriter, rt *route, who principal) {
	h.log.WarnContext(r.Context(), "control-plane request from a provisioned credential",
		"method", r.Method,
		"path", r.URL.Path,
		"client_addr", r.RemoteAddr,
		"user", who.User.ID,
	)
	audit.Log(r.Context(), "admin.ControlPlaneDenied",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.String("client_addr", r.RemoteAddr),
		slog.String("operation", rt.Summary),
		slog.Int("status", http.StatusForbidden),
	)
	httputil.WriteJSONError(w, http.StatusForbidden, msgForbidden)
}

// warnTokenDeprecated records that the shared admin token authorized an
// operation on object data, which a provisioned credential should be carrying
// instead.
//
// Only the data-plane routes warn. The control plane is what the token is still
// for, and warning there would bury the entries that matter under every
// dashboard status poll.
func (h *Handler) warnTokenDeprecated(r *http.Request) {
	h.log.WarnContext(r.Context(),
		"admin token authorized an object operation; issue a provisioned credential instead",
		"method", r.Method,
		"path", r.URL.Path,
		"client_addr", r.RemoteAddr,
	)
}
