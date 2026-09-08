// -------------------------------------------------------------------------------
// Ops - Bucket and Credential Provisioning
//
// Author: Alex Freidah
//
// Creating and removing the buckets, users, keypairs and grants a deployment
// holds in the store, and reading them back beside what the config file
// declares. Config-declared entries are visible and never editable: an operator
// reading that file has to be able to trust what it says.
//
// Every mutation ends by rebuilding the request-time registry, so a credential
// issued through the API authenticates on the next request rather than on the
// next restart.
// -------------------------------------------------------------------------------

package ops

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/provisioning"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// accessKeyBytes and secretBytes size the material a minted keypair carries.
// The access key is base32 of 12 bytes, which reads as 20 unpadded uppercase
// characters the way an AWS key does; the secret is base64 of 30, matching the
// 40 characters an S3 client expects to paste.
const (
	accessKeyBytes = 12
	secretBytes    = 30
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// ProvisioningDeps holds the collaborators Provisioning requires.
type ProvisioningDeps struct {
	Store    ProvisioningStore
	Objects  NamespaceCounter
	Registry RegistryPublisher
	Config   *ConfigStore
}

// Provisioning serves the bucket and credential administration shared by the
// admin API and the web UI.
type Provisioning struct {
	log      *slog.Logger
	store    ProvisioningStore
	objects  NamespaceCounter
	registry RegistryPublisher
	config   *ConfigStore
}

// NewProvisioning is the explicit-deps constructor.
func NewProvisioning(d ProvisioningDeps) *Provisioning {
	return &Provisioning{
		log:      slog.Default().With(logfmt.Component("ops")),
		store:    d.Store,
		objects:  d.Objects,
		registry: d.Registry,
		config:   d.Config,
	}
}

// NewCredential is a keypair as it exists exactly once: at the moment it is
// minted. Secret is returned to the caller that asked for it and never read
// back out of the store into any listing.
type NewCredential struct {
	AccessKeyID string
	Secret      string
	UserID      string
	Label       string
}

// -------------------------------------------------------------------------
// READS
// -------------------------------------------------------------------------

// View returns everything a deployment declares, from both sources, each entry
// carrying where it came from.
func (p *Provisioning) View(ctx context.Context) (provisioning.View, error) {
	return provisioning.LoadMerged(ctx, p.store, p.config.Load().Buckets)
}

// -------------------------------------------------------------------------
// BUCKETS
// -------------------------------------------------------------------------

// CreateBucket adds a virtual bucket to the store.
//
// The backend bucket it maps onto is not created here: which backends a
// deployment writes to, and what credentials reach them, is the operator's to
// configure. This declares the namespace the orchestrator will accept writes
// under.
func (p *Provisioning) CreateBucket(ctx context.Context, b *core.Bucket) error {
	if b.Name == "" {
		return ErrNameRequired
	}
	view, err := p.View(ctx)
	if err != nil {
		return err
	}
	if _, ok := findBucket(view.Buckets, b.Name); ok {
		return fmt.Errorf("%w: %q", ErrBucketExists, b.Name)
	}
	// Rejected here rather than at assembly: a rule the matcher cannot read
	// would otherwise store cleanly and then fail every registry rebuild,
	// including the one a reload runs, taking the fleet's reloads down.
	if errs := config.ValidateCORS(b.CORS); len(errs) > 0 {
		return fmt.Errorf("%w: %w", ErrInvalidCORS, errors.Join(errs...))
	}
	if err := p.store.CreateBucket(ctx, b); err != nil {
		return err
	}
	audit.Log(ctx, "provisioning.BucketCreated", slog.String("bucket", b.Name))
	return p.republish(ctx)
}

// DeleteBucket removes a virtual bucket.
//
// Refused while any object is stored under it: dropping the declaration would
// leave those keys addressable by nothing while still occupying every backend
// they were written to, so emptying the bucket stays a deliberate act. Grants
// naming it are refused for the same reason - a grant to a bucket that no
// longer exists is reported as dangling on every assembly until someone removes
// it.
func (p *Provisioning) DeleteBucket(ctx context.Context, name string) error {
	view, err := p.View(ctx)
	if err != nil {
		return err
	}
	b, ok := findBucket(view.Buckets, name)
	if !ok {
		return fmt.Errorf("%w: %q", ErrBucketNotFound, name)
	}
	if b.Source == provisioning.SourceConfig {
		return fmt.Errorf("%w: bucket %q", ErrConfigDeclared, name)
	}
	if err := p.refuseIfBucketInUse(ctx, name, &view); err != nil {
		return err
	}
	if err := p.store.DeleteBucket(ctx, name); err != nil {
		return err
	}
	audit.Log(ctx, "provisioning.BucketDeleted", slog.String("bucket", name))
	return p.republish(ctx)
}

// refuseIfBucketInUse reports the objects or grants that make a bucket
// undeletable.
func (p *Provisioning) refuseIfBucketInUse(ctx context.Context, name string, view *provisioning.View) error {
	n, err := p.objects.CountObjectsByPrefix(ctx, name+"/")
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%w: bucket %q holds %s", ErrBucketNotEmpty, name, plural(n, "object"))
	}
	for i := range view.Users {
		u := &view.Users[i]
		for _, granted := range u.Buckets {
			if granted == name {
				return fmt.Errorf("%w: bucket %q is granted to user %q", ErrBucketGranted, name, u.ID)
			}
		}
	}
	return nil
}

// -------------------------------------------------------------------------
// USERS
// -------------------------------------------------------------------------

// CreateUser adds an identity credentials can be issued against. The id is
// generated rather than taken from the caller, so it is unique and stable
// whatever the user is later renamed to.
func (p *Provisioning) CreateUser(ctx context.Context, name string) (core.User, error) {
	if name == "" {
		return core.User{}, ErrNameRequired
	}
	id, err := mintID("user")
	if err != nil {
		return core.User{}, err
	}
	u := core.User{ID: id, Name: name}
	if err := p.store.CreateUser(ctx, &u); err != nil {
		return core.User{}, err
	}
	audit.Log(ctx, "provisioning.UserCreated", slog.String("user", id), slog.String("name", name))
	return u, p.republish(ctx)
}

// DeleteUser removes an identity.
//
// Refused while the user still holds credentials or grants. The schema refuses
// it too, but a caller deserves to be told which of the two is in the way
// rather than a foreign-key violation.
func (p *Provisioning) DeleteUser(ctx context.Context, id string) error {
	view, err := p.View(ctx)
	if err != nil {
		return err
	}
	u, ok := findUser(view.Users, id)
	if !ok {
		return fmt.Errorf("%w: %q", ErrUserNotFound, id)
	}
	if u.Source == provisioning.SourceConfig {
		return fmt.Errorf("%w: user %q", ErrConfigDeclared, id)
	}
	if len(u.Buckets) > 0 {
		return fmt.Errorf("%w: user %q holds %s", ErrUserInUse, id, plural(int64(len(u.Buckets)), "grant"))
	}
	if n := countCredentials(view.Credentials, id); n > 0 {
		return fmt.Errorf("%w: user %q holds %s", ErrUserInUse, id, plural(int64(n), "credential"))
	}
	if err := p.store.DeleteUser(ctx, id); err != nil {
		return err
	}
	audit.Log(ctx, "provisioning.UserDeleted", slog.String("user", id))
	return p.republish(ctx)
}

// -------------------------------------------------------------------------
// CREDENTIALS
// -------------------------------------------------------------------------

// CreateCredential mints a keypair for an existing user and returns it, secret
// included, exactly once. Nothing reads the secret back out afterwards.
//
// The user is required rather than created on demand: a keypair with no owner
// is a credential nobody can account for, and issuing several to one identity
// is what lets one be replaced while its siblings keep working.
func (p *Provisioning) CreateCredential(ctx context.Context, userID, label string) (NewCredential, error) {
	if userID == "" {
		return NewCredential{}, ErrUserRequired
	}
	view, err := p.View(ctx)
	if err != nil {
		return NewCredential{}, err
	}
	u, ok := findUser(view.Users, userID)
	if !ok {
		return NewCredential{}, fmt.Errorf("%w: %q", ErrUserNotFound, userID)
	}
	if u.Source == provisioning.SourceConfig {
		return NewCredential{}, fmt.Errorf("%w: user %q", ErrConfigDeclared, userID)
	}

	accessKey, secret, err := mintKeypair()
	if err != nil {
		return NewCredential{}, err
	}
	row := core.Credential{AccessKeyID: accessKey, UserID: userID, Secret: secret, Label: label}
	if err := p.store.CreateCredential(ctx, &row); err != nil {
		return NewCredential{}, err
	}
	audit.Log(ctx, "provisioning.CredentialCreated",
		slog.String("user", userID), slog.String("access_key_id", accessKey))
	out := NewCredential{AccessKeyID: accessKey, Secret: secret, UserID: userID, Label: label}
	return out, p.republish(ctx)
}

// DeleteCredential revokes one keypair, leaving its siblings alone. Revocation
// takes effect on the next request rather than the next restart, because the
// registry is rebuilt before this returns.
func (p *Provisioning) DeleteCredential(ctx context.Context, accessKeyID string) error {
	view, err := p.View(ctx)
	if err != nil {
		return err
	}
	c, ok := findCredential(view.Credentials, accessKeyID)
	if !ok {
		return fmt.Errorf("%w: %q", ErrCredentialNotFound, accessKeyID)
	}
	if c.Source == provisioning.SourceConfig {
		return fmt.Errorf("%w: credential %q", ErrConfigDeclared, accessKeyID)
	}
	if err := p.store.DeleteCredential(ctx, accessKeyID); err != nil {
		return err
	}
	audit.Log(ctx, "provisioning.CredentialDeleted", slog.String("access_key_id", accessKeyID))
	return p.republish(ctx)
}

// -------------------------------------------------------------------------
// GRANTS
// -------------------------------------------------------------------------

// CreateGrant lets a user reach a bucket. The bucket may come from either
// source: granting a stored user access to a config-declared bucket is the
// normal way a deployment onboards a client onto a bucket it already runs.
func (p *Provisioning) CreateGrant(ctx context.Context, userID, bucketName string) error {
	view, err := p.View(ctx)
	if err != nil {
		return err
	}
	u, ok := findUser(view.Users, userID)
	if !ok {
		return fmt.Errorf("%w: %q", ErrUserNotFound, userID)
	}
	if u.Source == provisioning.SourceConfig {
		return fmt.Errorf("%w: user %q", ErrConfigDeclared, userID)
	}
	if _, ok := findBucket(view.Buckets, bucketName); !ok {
		return fmt.Errorf("%w: %q", ErrBucketNotFound, bucketName)
	}
	if err := p.store.CreateGrant(ctx, &core.Grant{UserID: userID, BucketName: bucketName}); err != nil {
		return err
	}
	audit.Log(ctx, "provisioning.GrantCreated",
		slog.String("user", userID), slog.String("bucket", bucketName))
	return p.republish(ctx)
}

// DeleteGrant withdraws one user's access to one bucket, leaving its other
// grants and every other user's alone.
func (p *Provisioning) DeleteGrant(ctx context.Context, userID, bucketName string) error {
	view, err := p.View(ctx)
	if err != nil {
		return err
	}
	u, ok := findUser(view.Users, userID)
	if !ok {
		return fmt.Errorf("%w: %q", ErrUserNotFound, userID)
	}
	if u.Source == provisioning.SourceConfig {
		return fmt.Errorf("%w: user %q", ErrConfigDeclared, userID)
	}
	if err := p.store.DeleteGrant(ctx, userID, bucketName); err != nil {
		return err
	}
	audit.Log(ctx, "provisioning.GrantDeleted",
		slog.String("user", userID), slog.String("bucket", bucketName))
	return p.republish(ctx)
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// republish rebuilds the request-time registry so the change takes effect now.
//
// A failure here is returned rather than swallowed: the row is written and the
// running registry is not, and a caller told the write succeeded would go on to
// use a credential that authenticates nothing.
func (p *Provisioning) republish(ctx context.Context) error {
	if p.registry == nil {
		return nil
	}
	if err := p.registry.Republish(ctx); err != nil {
		return fmt.Errorf("provisioning change was stored but the registry was not rebuilt: %w", err)
	}
	return nil
}

// plural renders a count and its noun, so a refusal naming one thing does not
// read as though it named several. Every message it serves is the reason an
// operator was just told no, which is a poor place to be sloppy.
func plural(n int64, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// findBucket returns the merged bucket with a name.
func findBucket(buckets []provisioning.Bucket, name string) (provisioning.Bucket, bool) {
	for i := range buckets {
		if buckets[i].Name == name {
			return buckets[i], true
		}
	}
	return provisioning.Bucket{}, false
}

// findUser returns the merged user with an id.
func findUser(users []provisioning.User, id string) (provisioning.User, bool) {
	for i := range users {
		if users[i].ID == id {
			return users[i], true
		}
	}
	return provisioning.User{}, false
}

// findCredential returns the merged credential with an access key.
func findCredential(creds []provisioning.Credential, accessKeyID string) (provisioning.Credential, bool) {
	for i := range creds {
		if creds[i].AccessKeyID == accessKeyID {
			return creds[i], true
		}
	}
	return provisioning.Credential{}, false
}

// countCredentials counts the merged credentials belonging to a user.
func countCredentials(creds []provisioning.Credential, userID string) int {
	n := 0
	for i := range creds {
		if creds[i].UserID == userID {
			n++
		}
	}
	return n
}

// mintKeypair draws a fresh access key and secret.
func mintKeypair() (accessKey, secret string, err error) {
	if accessKey, err = mintAccessKey(); err != nil {
		return "", "", err
	}
	raw := make([]byte, secretBytes)
	if _, err = rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("generate secret: %w", err)
	}
	return accessKey, base64.RawURLEncoding.EncodeToString(raw), nil
}

// mintAccessKey draws an access key in the uppercase alphanumeric form S3
// clients expect to see one in.
func mintAccessKey() (string, error) {
	raw := make([]byte, accessKeyBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate access key: %w", err)
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw), nil
}

// mintID draws an opaque identifier under a kind prefix, so a value that turns
// up in a log or an audit record says what it names.
func mintID(kind string) (string, error) {
	raw := make([]byte, accessKeyBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate %s id: %w", kind, err)
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	return kind + "-" + strings.ToLower(enc), nil
}
