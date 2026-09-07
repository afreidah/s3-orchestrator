// -------------------------------------------------------------------------------
// Postgres Store - Bucket and Credential Provisioning
//
// Author: Alex Freidah
//
// The store half of the bucket registry: buckets, the users that reach them, the
// keypairs those users authenticate with, and the grants pairing the two. The
// listings are what registry assembly reads before merging with what config
// declares; the rest is how each row comes into being and stops being.
// -------------------------------------------------------------------------------

package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

// -------------------------------------------------------------------------
// LISTINGS
// -------------------------------------------------------------------------

// ListBuckets returns every stored bucket, ordered by name.
func (s *Store) ListBuckets(ctx context.Context) ([]core.Bucket, error) {
	rows, err := s.queries.ListBuckets(ctx)
	if err != nil {
		return nil, fmt.Errorf("list buckets: %w", err)
	}
	out := make([]core.Bucket, 0, len(rows))
	for i := range rows {
		b, err := bucketFromRow(&rows[i])
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// ListUsers returns every stored user, ordered by name.
func (s *Store) ListUsers(ctx context.Context) ([]core.User, error) {
	rows, err := s.queries.ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return mapSlice(rows, userFromRow), nil
}

// ListCredentials returns every stored keypair, ordered by access key.
func (s *Store) ListCredentials(ctx context.Context) ([]core.Credential, error) {
	rows, err := s.queries.ListCredentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("list credentials: %w", err)
	}
	return mapSlice(rows, credentialFromRow), nil
}

// ListGrants returns every stored grant, ordered by user then bucket.
func (s *Store) ListGrants(ctx context.Context) ([]core.Grant, error) {
	rows, err := s.queries.ListGrants(ctx)
	if err != nil {
		return nil, fmt.Errorf("list grants: %w", err)
	}
	return mapSlice(rows, grantFromRow), nil
}

// -------------------------------------------------------------------------
// WRITES
// -------------------------------------------------------------------------

// CreateBucket inserts a bucket.
func (s *Store) CreateBucket(ctx context.Context, b *core.Bucket) error {
	cors, err := marshalCORS(b.CORS)
	if err != nil {
		return err
	}
	if err := s.queries.CreateBucket(ctx, db.CreateBucketParams{
		Name:                b.Name,
		MaxMultipartUploads: int32(b.MaxMultipartUploads), //nolint:gosec // G115: bounded by config validation
		Cors:                cors,
	}); err != nil {
		return fmt.Errorf("create bucket %s: %w", b.Name, err)
	}
	return nil
}

// CreateUser inserts a user.
func (s *Store) CreateUser(ctx context.Context, u *core.User) error {
	if err := s.queries.CreateUser(ctx, db.CreateUserParams{
		ID:   u.ID,
		Name: u.Name,
	}); err != nil {
		return fmt.Errorf("create user %s: %w", u.Name, err)
	}
	return nil
}

// CreateCredential inserts a keypair against an existing user.
//
// The error names the access key rather than the secret, which never reaches a
// log, an error string or an audit record on any path.
func (s *Store) CreateCredential(ctx context.Context, c *core.Credential) error {
	if err := s.queries.CreateCredential(ctx, db.CreateCredentialParams{
		AccessKeyID: c.AccessKeyID,
		UserID:      c.UserID,
		Secret:      c.Secret,
		Label:       nullableString(c.Label),
		Disabled:    c.Disabled,
	}); err != nil {
		return fmt.Errorf("create credential %s: %w", c.AccessKeyID, err)
	}
	return nil
}

// CreateGrant gives a user access to one bucket.
func (s *Store) CreateGrant(ctx context.Context, g *core.Grant) error {
	if err := s.queries.CreateGrant(ctx, db.CreateGrantParams{
		UserID:     g.UserID,
		BucketName: g.BucketName,
	}); err != nil {
		return fmt.Errorf("create grant %s -> %s: %w", g.UserID, g.BucketName, err)
	}
	return nil
}

// DeleteBucket removes a stored bucket. Objects under it are unaffected, so a
// caller that means to destroy data does that first.
func (s *Store) DeleteBucket(ctx context.Context, name string) error {
	if err := s.queries.DeleteBucket(ctx, name); err != nil {
		return fmt.Errorf("delete bucket %s: %w", name, err)
	}
	return nil
}

// DeleteUser removes a user. The foreign keys refuse while it still holds
// credentials or grants, so those are removed first and revocation stays an
// explicit act.
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	if err := s.queries.DeleteUser(ctx, id); err != nil {
		return fmt.Errorf("delete user %s: %w", id, err)
	}
	return nil
}

// DeleteCredential revokes one keypair, leaving its siblings working.
func (s *Store) DeleteCredential(ctx context.Context, accessKeyID string) error {
	if err := s.queries.DeleteCredential(ctx, accessKeyID); err != nil {
		return fmt.Errorf("delete credential %s: %w", accessKeyID, err)
	}
	return nil
}

// DeleteGrant withdraws a user's access to one bucket, leaving its other grants
// in place.
func (s *Store) DeleteGrant(ctx context.Context, userID, bucketName string) error {
	if err := s.queries.DeleteGrant(ctx, db.DeleteGrantParams{
		UserID:     userID,
		BucketName: bucketName,
	}); err != nil {
		return fmt.Errorf("delete grant %s -> %s: %w", userID, bucketName, err)
	}
	return nil
}

// -------------------------------------------------------------------------
// ROW CONVERSION
// -------------------------------------------------------------------------

// bucketFromRow converts a sqlc buckets row into the canonical core.Bucket.
func bucketFromRow(r *db.Bucket) (core.Bucket, error) {
	cors, err := unmarshalCORS(r.Cors)
	if err != nil {
		return core.Bucket{}, err
	}
	return core.Bucket{
		Name:                r.Name,
		MaxMultipartUploads: int(r.MaxMultipartUploads),
		CORS:                cors,
		CreatedAt:           r.CreatedAt.Time,
	}, nil
}

// userFromRow converts a sqlc users row into the canonical type.
func userFromRow(r *db.User) core.User {
	return core.User{
		ID:        r.ID,
		Name:      r.Name,
		CreatedAt: r.CreatedAt.Time,
	}
}

// credentialFromRow converts a sqlc credentials row into the canonical type.
func credentialFromRow(r *db.Credential) core.Credential {
	label := ""
	if r.Label != nil {
		label = *r.Label
	}
	return core.Credential{
		AccessKeyID: r.AccessKeyID,
		UserID:      r.UserID,
		Secret:      r.Secret,
		Label:       label,
		Disabled:    r.Disabled,
		CreatedAt:   r.CreatedAt.Time,
		LastUsedAt:  timestamptzPtr(r.LastUsedAt),
	}
}

// grantFromRow converts a sqlc grants row into the canonical type.
func grantFromRow(r *db.Grant) core.Grant {
	return core.Grant{
		UserID:     r.UserID,
		BucketName: r.BucketName,
		CreatedAt:  r.CreatedAt.Time,
	}
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// nullableString returns a *string for a column that stores NULL rather than
// the empty value, which label does when a credential carries no name.
func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// marshalCORS renders a bucket's browser rules for storage. A bucket with no
// rules stores NULL rather than an empty array, so "no CORS configured" is one
// value in the column rather than two.
func marshalCORS(rules []config.CORSRule) ([]byte, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(rules)
	if err != nil {
		return nil, fmt.Errorf("encode bucket cors: %w", err)
	}
	return encoded, nil
}

// unmarshalCORS reads a bucket's browser rules back.
func unmarshalCORS(raw []byte) ([]config.CORSRule, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var rules []config.CORSRule
	if err := json.Unmarshal(raw, &rules); err != nil {
		return nil, fmt.Errorf("decode bucket cors: %w", err)
	}
	return rules, nil
}
