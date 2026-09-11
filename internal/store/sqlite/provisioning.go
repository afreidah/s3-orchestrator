// -------------------------------------------------------------------------------
// SQLite Store - Bucket and Credential Provisioning
//
// Author: Alex Freidah
//
// The store half of the bucket registry: buckets, the users that reach them, the
// keypairs those users authenticate with, and the grants pairing the two. The
// listings are what registry assembly reads before merging with what config
// declares; the rest is how each row comes into being and stops being.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// LISTINGS
// -------------------------------------------------------------------------

// ListBuckets returns every stored bucket, ordered by name.
//
// Every listing here orders in SQL rather than in Go so both engines hand back
// the same sequence and a caller comparing two assemblies is comparing content
// rather than ordering.
func (s *Store) ListBuckets(ctx context.Context) ([]core.Bucket, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, max_multipart_uploads, cors, created_at
		 FROM buckets
		 ORDER BY name`,
	)
	if err != nil {
		return nil, fmt.Errorf("list buckets: %w", err)
	}
	return collectRows(rows, "buckets", scanBucket)
}

// ListUsers returns every stored user, ordered by name.
func (s *Store) ListUsers(ctx context.Context) ([]core.User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, created_at
		 FROM users
		 ORDER BY name`,
	)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return collectRows(rows, "users", scanUser)
}

// ListCredentials returns every stored keypair, ordered by access key.
//
// Disabled credentials are included: whether one authenticates is the
// registry's decision, and a listing that hid them would also hide them from
// the operator asking what exists.
func (s *Store) ListCredentials(ctx context.Context) ([]core.Credential, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT access_key_id, user_id, secret, label, disabled, created_at, last_used_at
		 FROM credentials
		 ORDER BY access_key_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("list credentials: %w", err)
	}
	return collectRows(rows, "credentials", scanCredential)
}

// ListGrants returns every stored grant, ordered by user then bucket.
func (s *Store) ListGrants(ctx context.Context) ([]core.Grant, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT user_id, bucket_name, permissions, created_at
		 FROM grants
		 ORDER BY user_id, bucket_name`,
	)
	if err != nil {
		return nil, fmt.Errorf("list grants: %w", err)
	}
	return collectRows(rows, "grants", scanGrant)
}

// -------------------------------------------------------------------------
// WRITES
// -------------------------------------------------------------------------

// CreateBucket inserts a bucket.
//
// created_at is written here rather than left to the column default, as every
// other timestamp write is: the default renders milliseconds while these render
// nanoseconds, and the two widths do not compare as text.
func (s *Store) CreateBucket(ctx context.Context, b *core.Bucket) error {
	cors, err := marshalCORS(b.CORS)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO buckets (name, max_multipart_uploads, cors, created_at)
		 VALUES (?, ?, ?, ?)`,
		b.Name, b.MaxMultipartUploads, cors, now(),
	); err != nil {
		return fmt.Errorf("create bucket %s: %w", b.Name, err)
	}
	return nil
}

// CreateUser inserts a user.
func (s *Store) CreateUser(ctx context.Context, u *core.User) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id, name, created_at) VALUES (?, ?, ?)`,
		u.ID, u.Name, now(),
	); err != nil {
		return fmt.Errorf("create user %s: %w", u.Name, err)
	}
	return nil
}

// CreateCredential inserts a keypair against an existing user.
//
// The error names the access key rather than the secret, which never reaches a
// log, an error string or an audit record on any path.
func (s *Store) CreateCredential(ctx context.Context, c *core.Credential) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO credentials
		   (access_key_id, user_id, secret, label, disabled, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		c.AccessKeyID, c.UserID, c.Secret,
		nullableString(c.Label), boolToInt(c.Disabled), now(),
	); err != nil {
		return fmt.Errorf("create credential %s: %w", c.AccessKeyID, err)
	}
	return nil
}

// CreateGrant gives a user access to one bucket.
func (s *Store) CreateGrant(ctx context.Context, g *core.Grant) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO grants (user_id, bucket_name, permissions, created_at) VALUES (?, ?, ?, ?)`,
		g.UserID, g.BucketName, g.Permissions.String(), now(),
	); err != nil {
		return fmt.Errorf("create grant %s -> %s: %w", g.UserID, g.BucketName, err)
	}
	return nil
}

// DeleteBucket removes a stored bucket. Objects under it are unaffected, so a
// caller that means to destroy data does that first.
func (s *Store) DeleteBucket(ctx context.Context, name string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM buckets WHERE name = ?`, name); err != nil {
		return fmt.Errorf("delete bucket %s: %w", name, err)
	}
	return nil
}

// DeleteUser removes a user. The schema refuses while it still holds credentials
// or grants, so those are removed first and revocation stays an explicit act.
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete user %s: %w", id, err)
	}
	return nil
}

// DeleteCredential revokes one keypair, leaving its siblings working.
func (s *Store) DeleteCredential(ctx context.Context, accessKeyID string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM credentials WHERE access_key_id = ?`, accessKeyID,
	); err != nil {
		return fmt.Errorf("delete credential %s: %w", accessKeyID, err)
	}
	return nil
}

// DeleteGrant withdraws a user's access to one bucket, leaving its other grants
// in place.
func (s *Store) DeleteGrant(ctx context.Context, userID, bucketName string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM grants WHERE user_id = ? AND bucket_name = ?`,
		userID, bucketName,
	); err != nil {
		return fmt.Errorf("delete grant %s -> %s: %w", userID, bucketName, err)
	}
	return nil
}

// -------------------------------------------------------------------------
// SCANNING
// -------------------------------------------------------------------------

// scanBucket reads one buckets row.
func scanBucket(rows *sql.Rows) (core.Bucket, error) {
	var (
		b       core.Bucket
		cors    sql.NullString
		created string
	)
	if err := rows.Scan(&b.Name, &b.MaxMultipartUploads, &cors, &created); err != nil {
		return core.Bucket{}, fmt.Errorf("scan bucket: %w", err)
	}
	var err error
	if b.CORS, err = unmarshalCORS(cors); err != nil {
		return core.Bucket{}, err
	}
	if b.CreatedAt, err = parseTime(created); err != nil {
		return core.Bucket{}, fmt.Errorf("parse bucket created_at: %w", err)
	}
	return b, nil
}

// scanUser reads one users row.
func scanUser(rows *sql.Rows) (core.User, error) {
	var (
		u       core.User
		created string
	)
	if err := rows.Scan(&u.ID, &u.Name, &created); err != nil {
		return core.User{}, fmt.Errorf("scan user: %w", err)
	}
	var err error
	if u.CreatedAt, err = parseTime(created); err != nil {
		return core.User{}, fmt.Errorf("parse user created_at: %w", err)
	}
	return u, nil
}

// scanCredential reads one credentials row.
func scanCredential(rows *sql.Rows) (core.Credential, error) {
	var (
		c        core.Credential
		label    sql.NullString
		disabled int
		created  string
		lastUsed sql.NullString
	)
	if err := rows.Scan(&c.AccessKeyID, &c.UserID, &c.Secret,
		&label, &disabled, &created, &lastUsed); err != nil {
		return core.Credential{}, fmt.Errorf("scan credential: %w", err)
	}
	c.Label = nullStringValue(label)
	c.Disabled = disabled != 0

	var err error
	if c.CreatedAt, err = parseTime(created); err != nil {
		return core.Credential{}, fmt.Errorf("parse credential created_at: %w", err)
	}
	c.LastUsedAt = parseNullableTime(lastUsed)
	return c, nil
}

// scanGrant reads one grants row.
func scanGrant(rows *sql.Rows) (core.Grant, error) {
	var (
		g       core.Grant
		perms   string
		created string
	)
	if err := rows.Scan(&g.UserID, &g.BucketName, &perms, &created); err != nil {
		return core.Grant{}, fmt.Errorf("scan grant: %w", err)
	}
	var err error
	// A value nothing recognises fails the read rather than resolving to some
	// set. Falling back to full access would grant what nobody wrote down, and
	// to none would refuse a caller the operator authorized.
	if g.Permissions, err = core.ParsePermissions(perms); err != nil {
		return core.Grant{}, fmt.Errorf("grant %s -> %s: %w", g.UserID, g.BucketName, err)
	}
	if g.CreatedAt, err = parseTime(created); err != nil {
		return core.Grant{}, fmt.Errorf("parse grant created_at: %w", err)
	}
	return g, nil
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// marshalCORS renders a bucket's browser rules for storage. A bucket with no
// rules stores NULL rather than an empty array, so "no CORS configured" is one
// value in the column rather than two.
func marshalCORS(rules []config.CORSRule) (sql.NullString, error) {
	if len(rules) == 0 {
		return sql.NullString{}, nil
	}
	encoded, err := json.Marshal(rules)
	if err != nil {
		return sql.NullString{}, fmt.Errorf("encode bucket cors: %w", err)
	}
	return sql.NullString{String: string(encoded), Valid: true}, nil
}

// unmarshalCORS reads a bucket's browser rules back.
func unmarshalCORS(s sql.NullString) ([]config.CORSRule, error) {
	if !s.Valid || s.String == "" {
		return nil, nil
	}
	var rules []config.CORSRule
	if err := json.Unmarshal([]byte(s.String), &rules); err != nil {
		return nil, fmt.Errorf("decode bucket cors: %w", err)
	}
	return rules, nil
}
