// -------------------------------------------------------------------------------
// Admin API - Bucket and Credential Provisioning
//
// Author: Alex Freidah
//
// Declaring buckets, users, keypairs and grants over HTTP, and reading back
// what a deployment holds from both sources at once. The work is
// ops.Provisioning; these parse, call it, and map its rejections onto statuses.
//
// A rejection is a client error, not a fault: a bucket that still holds objects
// or an entry the config file declares are both answers, and the caller is told
// which so it can act rather than retry.
// -------------------------------------------------------------------------------

package admin

import (
	"errors"
	"net/http"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/provisioning"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// provisioningBodyLimit caps a provisioning request body. These carry names and
// a CORS rule set, never data.
const provisioningBodyLimit = 64 << 10

// -------------------------------------------------------------------------
// READS
// -------------------------------------------------------------------------

// handleProvisioning returns every bucket, user and credential a deployment
// declares, from both the config file and the store.
func (h *Handler) handleProvisioning(w http.ResponseWriter, r *http.Request) {
	view, err := h.provision.View(r.Context())
	if err != nil {
		h.internalError(r.Context(), w, "provisioning listing failed", err)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, provisioningResponse(&view))
}

// -------------------------------------------------------------------------
// BUCKETS
// -------------------------------------------------------------------------

// handleCreateBucket declares a virtual bucket.
func (h *Handler) handleCreateBucket(w http.ResponseWriter, r *http.Request) {
	var req adminapi.CreateBucketRequest
	if !httputil.DecodeJSONBody(w, r, &req, provisioningBodyLimit) {
		return
	}
	b := core.Bucket{
		Name:                req.Name,
		MaxMultipartUploads: req.MaxMultipartUploads,
		CORS:                configCORS(req.CORS),
	}
	if err := h.provision.CreateBucket(r.Context(), &b); err != nil {
		h.provisioningError(w, r, "create bucket failed", err)
		return
	}
	httputil.WriteJSON(w, http.StatusCreated, adminapi.ProvisioningOperationResponse{
		Status: statusOK, Bucket: req.Name,
	})
}

// handleDeleteBucket removes a virtual bucket that holds nothing.
func (h *Handler) handleDeleteBucket(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue(paramName)
	if err := h.provision.DeleteBucket(r.Context(), name); err != nil {
		h.provisioningError(w, r, "delete bucket failed", err)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, adminapi.ProvisioningOperationResponse{
		Status: statusOK, Bucket: name,
	})
}

// -------------------------------------------------------------------------
// USERS
// -------------------------------------------------------------------------

// handleCreateUser declares an identity credentials can be issued against.
func (h *Handler) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req adminapi.CreateUserRequest
	if !httputil.DecodeJSONBody(w, r, &req, provisioningBodyLimit) {
		return
	}
	u, err := h.provision.CreateUser(r.Context(), req.Name)
	if err != nil {
		h.provisioningError(w, r, "create user failed", err)
		return
	}
	httputil.WriteJSON(w, http.StatusCreated, adminapi.ProvisioningOperationResponse{
		Status: statusOK, UserID: u.ID, UserName: u.Name,
	})
}

// handleDeleteUser removes an identity that holds nothing.
func (h *Handler) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue(paramID)
	if err := h.provision.DeleteUser(r.Context(), id); err != nil {
		h.provisioningError(w, r, "delete user failed", err)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, adminapi.ProvisioningOperationResponse{
		Status: statusOK, UserID: id,
	})
}

// -------------------------------------------------------------------------
// CREDENTIALS
// -------------------------------------------------------------------------

// handleCreateCredential mints a keypair and returns it once. The secret is in
// this response and nowhere else, so a caller that loses it issues a
// replacement rather than recovering this one.
func (h *Handler) handleCreateCredential(w http.ResponseWriter, r *http.Request) {
	var req adminapi.CreateCredentialRequest
	if !httputil.DecodeJSONBody(w, r, &req, provisioningBodyLimit) {
		return
	}
	c, err := h.provision.CreateCredential(r.Context(), req.UserID, req.Label)
	if err != nil {
		h.provisioningError(w, r, "create credential failed", err)
		return
	}
	httputil.WriteJSON(w, http.StatusCreated, adminapi.CreateCredentialResponse{
		AccessKeyID:     c.AccessKeyID,
		SecretAccessKey: c.Secret,
		UserID:          c.UserID,
		Label:           c.Label,
	})
}

// handleDeleteCredential revokes one keypair, leaving its siblings alone.
func (h *Handler) handleDeleteCredential(w http.ResponseWriter, r *http.Request) {
	accessKeyID := r.PathValue(paramID)
	if err := h.provision.DeleteCredential(r.Context(), accessKeyID); err != nil {
		h.provisioningError(w, r, "delete credential failed", err)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, adminapi.ProvisioningOperationResponse{Status: statusOK})
}

// -------------------------------------------------------------------------
// GRANTS
// -------------------------------------------------------------------------

// handleCreateGrant lets a user reach a bucket.
func (h *Handler) handleCreateGrant(w http.ResponseWriter, r *http.Request) {
	var req adminapi.CreateGrantRequest
	if !httputil.DecodeJSONBody(w, r, &req, provisioningBodyLimit) {
		return
	}
	perms, err := core.ParsePermissions(strings.Join(req.Permissions, ","))
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.provision.CreateGrant(r.Context(), req.UserID, req.Bucket, perms); err != nil {
		h.provisioningError(w, r, "create grant failed", err)
		return
	}
	httputil.WriteJSON(w, http.StatusCreated, adminapi.ProvisioningOperationResponse{
		Status: statusOK, UserID: req.UserID, Bucket: req.Bucket,
	})
}

// handleDeleteGrant withdraws one user's access to one bucket.
func (h *Handler) handleDeleteGrant(w http.ResponseWriter, r *http.Request) {
	userID, bucket := r.PathValue(paramID), r.PathValue(paramName)
	if err := h.provision.DeleteGrant(r.Context(), userID, bucket); err != nil {
		h.provisioningError(w, r, "delete grant failed", err)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, adminapi.ProvisioningOperationResponse{
		Status: statusOK, UserID: userID, Bucket: bucket,
	})
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// provisioningError maps an operation's rejection onto a status. Everything the
// operations layer names is something the caller stated or asked for, so it is
// reported with its own reason; anything else is a fault and says nothing.
func (h *Handler) provisioningError(w http.ResponseWriter, r *http.Request, msg string, err error) {
	switch {
	case errors.Is(err, ops.ErrNameRequired),
		errors.Is(err, ops.ErrUserRequired),
		errors.Is(err, ops.ErrNoPermissions),
		errors.Is(err, ops.ErrInvalidCORS):
		httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ops.ErrBucketNotFound),
		errors.Is(err, ops.ErrUserNotFound),
		errors.Is(err, ops.ErrCredentialNotFound):
		httputil.WriteJSONError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ops.ErrConfigDeclared):
		httputil.WriteJSONError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, ops.ErrBucketExists),
		errors.Is(err, ops.ErrBucketNotEmpty),
		errors.Is(err, ops.ErrBucketGranted),
		errors.Is(err, ops.ErrUserInUse):
		httputil.WriteJSONError(w, http.StatusConflict, err.Error())
	default:
		h.internalError(r.Context(), w, msg, err)
	}
}

// provisioningResponse renders the merged view. Secrets are dropped here rather
// than filtered later: the wire type has no field to carry one.
func provisioningResponse(v *provisioning.View) adminapi.ProvisioningResponse {
	out := adminapi.ProvisioningResponse{
		Buckets:     make([]adminapi.Bucket, 0, len(v.Buckets)),
		Users:       make([]adminapi.User, 0, len(v.Users)),
		Credentials: make([]adminapi.Credential, 0, len(v.Credentials)),
	}
	for i := range v.Buckets {
		b := &v.Buckets[i]
		out.Buckets = append(out.Buckets, adminapi.Bucket{
			Name:                b.Name,
			MaxMultipartUploads: b.MaxMultipartUploads,
			CORS:                wireCORS(b.CORS),
			Source:              string(b.Source),
		})
	}
	for i := range v.Users {
		u := &v.Users[i]
		out.Users = append(out.Users, adminapi.User{
			ID:      u.ID,
			Name:    u.Name,
			Buckets: u.Buckets,
			Grants:  wireGrants(u.Buckets, u.Grants),
			Source:  string(u.Source),
		})
	}
	for i := range v.Credentials {
		c := &v.Credentials[i]
		if c.AccessKeyID == "" {
			continue
		}
		out.Credentials = append(out.Credentials, adminapi.Credential{
			AccessKeyID: c.AccessKeyID,
			UserID:      c.UserID,
			Label:       c.Label,
			Source:      string(c.Source),
		})
	}
	for _, n := range v.Notices {
		out.Notices = append(out.Notices, adminapi.Notice{Kind: n.Kind, Detail: n.Detail})
	}
	return out
}

// wireGrants renders a user's grants, ordered by the bucket list so a listing
// is stable between reads rather than following map iteration.
func wireGrants(buckets []string, grants map[string]core.PermissionSet) []adminapi.Grant {
	out := make([]adminapi.Grant, 0, len(buckets))
	for _, name := range buckets {
		out = append(out, adminapi.Grant{
			Bucket:      name,
			Permissions: grants[name].Names(),
		})
	}
	return out
}

// wireCORS converts a bucket's rules onto the wire.
func wireCORS(rules []config.CORSRule) []adminapi.CORSRule {
	if len(rules) == 0 {
		return nil
	}
	out := make([]adminapi.CORSRule, 0, len(rules))
	for i := range rules {
		r := &rules[i]
		out = append(out, adminapi.CORSRule{
			AllowedOrigins: r.AllowedOrigins,
			AllowedMethods: r.AllowedMethods,
			AllowedHeaders: r.AllowedHeaders,
			ExposeHeaders:  r.ExposeHeaders,
			MaxAge:         r.MaxAge,
		})
	}
	return out
}

// configCORS converts submitted rules into the config shape a bucket stores.
func configCORS(rules []adminapi.CORSRule) []config.CORSRule {
	if len(rules) == 0 {
		return nil
	}
	out := make([]config.CORSRule, 0, len(rules))
	for i := range rules {
		r := &rules[i]
		out = append(out, config.CORSRule{
			AllowedOrigins: r.AllowedOrigins,
			AllowedMethods: r.AllowedMethods,
			AllowedHeaders: r.AllowedHeaders,
			ExposeHeaders:  r.ExposeHeaders,
			MaxAge:         r.MaxAge,
		})
	}
	return out
}
