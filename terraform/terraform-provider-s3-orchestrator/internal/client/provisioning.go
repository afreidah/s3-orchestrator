// -------------------------------------------------------------------------------
// Admin API Client - Provisioning
//
// Author: Alex Freidah
//
// The provisioning half of the admin API: users, the keypairs that prove them,
// and the grants that say what each one reaches. One listing endpoint answers
// for all of it, so every read here is a fetch of that document and a search
// through it rather than a lookup of one resource.
//
// Grants arrive nested under the user that holds them and carry no source of
// their own, so whether a grant may be changed follows the user it hangs off.
// -------------------------------------------------------------------------------

package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// The provisioning routes.
const (
	pathProvisioning = "/admin/api/provisioning"
	pathUsers        = pathProvisioning + "/users"
	pathCredentials  = pathProvisioning + "/credentials"
	pathGrants       = pathProvisioning + "/grants"
)

// SourceConfig marks an entry the orchestrator's config file declares. Those
// are readable through the API and never writable through it.
const SourceConfig = "config"

// KindOrchestrator is the resource a control-plane grant names when it belongs
// to no single bucket or backend. It carries no name.
const KindOrchestrator = "orchestrator"

// -------------------------------------------------------------------------
// WIRE TYPES
// -------------------------------------------------------------------------

// Grant is one resource a user reaches and the permissions that reach carries.
type Grant struct {
	Kind        string   `json:"kind"`
	Name        string   `json:"name,omitempty"`
	Permissions []string `json:"permissions"`
}

// User is an identity credentials prove and grants empower.
type User struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Buckets []string `json:"buckets"`
	Grants  []Grant  `json:"grants"`
	Source  string   `json:"source"`
}

// Credential is one keypair, rendered without its secret: the listing never
// carries one.
type Credential struct {
	AccessKeyID string `json:"access_key_id"`
	UserID      string `json:"user_id"`
	Label       string `json:"label,omitempty"`
	Source      string `json:"source"`
}

// Bucket is a virtual bucket either source declares.
type Bucket struct {
	Name                string `json:"name"`
	MaxMultipartUploads int    `json:"max_multipart_uploads"`
	Source              string `json:"source"`
}

// Provisioning is everything the deployment declares, from both sources at
// once.
type Provisioning struct {
	Buckets     []Bucket     `json:"buckets"`
	Users       []User       `json:"users"`
	Credentials []Credential `json:"credentials"`
}

// CreateUserRequest declares an identity.
type CreateUserRequest struct {
	Name string `json:"name"`
}

// RenameUserRequest changes the name an identity is read by. The ID is in the
// path and does not move.
type RenameUserRequest struct {
	Name string `json:"name"`
}

// CreateCredentialRequest registers a keypair against a user. Supplying both
// halves records that keypair; supplying neither mints one.
type CreateCredentialRequest struct {
	UserID          string `json:"user_id"`
	Label           string `json:"label,omitempty"`
	AccessKeyID     string `json:"access_key_id,omitempty"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`
}

// CreateCredentialResponse carries the keypair the request produced, minted or
// echoed back.
type CreateCredentialResponse struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	UserID          string `json:"user_id"`
	Label           string `json:"label,omitempty"`
}

// SetGrantRequest declares exactly what a user reaches on one resource.
type SetGrantRequest struct {
	Permissions []string `json:"permissions"`
}

// OperationResponse is the acknowledgement a mutation answers with.
type OperationResponse struct {
	Status   string `json:"status"`
	UserID   string `json:"user_id,omitempty"`
	UserName string `json:"user_name,omitempty"`
	Resource string `json:"resource,omitempty"`
}

// -------------------------------------------------------------------------
// LISTING
// -------------------------------------------------------------------------

// Provisioning fetches everything the deployment declares.
func (c *Client) Provisioning(ctx context.Context) (*Provisioning, error) {
	var out Provisioning
	if err := c.Do(ctx, http.MethodGet, pathProvisioning, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// User finds one identity by id. The second result reports whether it exists,
// which is what tells a Read to drop the resource rather than fail the run.
func (c *Client) User(ctx context.Context, id string) (User, bool, error) {
	p, err := c.Provisioning(ctx)
	if err != nil {
		return User{}, false, err
	}
	for i := range p.Users {
		if p.Users[i].ID == id {
			return p.Users[i], true, nil
		}
	}
	return User{}, false, nil
}

// Credential finds one keypair by access key. The secret is never part of the
// answer: nothing reads it back out of the orchestrator.
func (c *Client) Credential(ctx context.Context, accessKeyID string) (Credential, bool, error) {
	p, err := c.Provisioning(ctx)
	if err != nil {
		return Credential{}, false, err
	}
	for i := range p.Credentials {
		if p.Credentials[i].AccessKeyID == accessKeyID {
			return p.Credentials[i], true, nil
		}
	}
	return Credential{}, false, nil
}

// Grant finds one user's grant on one resource.
//
// Grants are nested under the user rather than listed on their own, so a grant
// whose user is gone reports as absent rather than as an error: from the
// caller's side the grant is equally not there.
func (c *Client) Grant(ctx context.Context, userID, kind, name string) (Grant, bool, error) {
	u, ok, err := c.User(ctx, userID)
	if err != nil || !ok {
		return Grant{}, false, err
	}
	for i := range u.Grants {
		if u.Grants[i].Kind == kind && u.Grants[i].Name == name {
			return u.Grants[i], true, nil
		}
	}
	return Grant{}, false, nil
}

// -------------------------------------------------------------------------
// USERS
// -------------------------------------------------------------------------

// CreateUser declares an identity and returns the id the server generated,
// which every later call names it by.
func (c *Client) CreateUser(ctx context.Context, name string) (OperationResponse, error) {
	var out OperationResponse
	err := c.Do(ctx, http.MethodPost, pathUsers, CreateUserRequest{Name: name}, &out)
	return out, err
}

// RenameUser changes the name an identity is read by, leaving the id its
// credentials and grants reference in place.
func (c *Client) RenameUser(ctx context.Context, id, name string) error {
	return c.Do(ctx, http.MethodPatch, pathUsers+"/"+url.PathEscape(id),
		RenameUserRequest{Name: name}, nil)
}

// DeleteUser removes an identity. Refused while it still holds a credential or
// a grant, so those come off first.
func (c *Client) DeleteUser(ctx context.Context, id string) error {
	return c.Do(ctx, http.MethodDelete, pathUsers+"/"+url.PathEscape(id), nil, nil)
}

// -------------------------------------------------------------------------
// CREDENTIALS
// -------------------------------------------------------------------------

// CreateCredential records a keypair against a user, minting one when the
// request supplies none.
func (c *Client) CreateCredential(
	ctx context.Context, req CreateCredentialRequest,
) (CreateCredentialResponse, error) {
	var out CreateCredentialResponse
	err := c.Do(ctx, http.MethodPost, pathCredentials, req, &out)
	return out, err
}

// DeleteCredential revokes one keypair, leaving its siblings working.
func (c *Client) DeleteCredential(ctx context.Context, accessKeyID string) error {
	return c.Do(ctx, http.MethodDelete, pathCredentials+"/"+url.PathEscape(accessKeyID), nil, nil)
}

// -------------------------------------------------------------------------
// GRANTS
// -------------------------------------------------------------------------

// SetGrant declares exactly what a user reaches on one resource, writing the
// grant when it is absent and replacing its permissions when it is not.
//
// Upsert rather than create, so an apply does not have to know whether the
// grant is already there and re-applying converges.
func (c *Client) SetGrant(ctx context.Context, userID, kind, name string, permissions []string) error {
	return c.Do(ctx, http.MethodPut, grantPath(userID, kind, name),
		SetGrantRequest{Permissions: permissions}, nil)
}

// DeleteGrant withdraws one user's access to one resource.
func (c *Client) DeleteGrant(ctx context.Context, userID, kind, name string) error {
	return c.Do(ctx, http.MethodDelete, grantPath(userID, kind, name), nil, nil)
}

// grantPath addresses one grant.
//
// The orchestrator carries no name, so the path takes a placeholder segment the
// server discards once the kind in the query says which resource is meant.
func grantPath(userID, kind, name string) string {
	segment := name
	if segment == "" {
		segment = KindOrchestrator
	}
	return fmt.Sprintf("%s/%s/%s?kind=%s",
		pathGrants, url.PathEscape(userID), url.PathEscape(segment), url.QueryEscape(kind))
}
