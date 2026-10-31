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
		`SELECT user_id, resource_kind, resource_name, permissions, created_at
		 FROM grants
		 ORDER BY user_id, resource_kind, resource_name`,
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
	return s.exec(ctx, "create bucket "+b.Name,
		`INSERT INTO buckets (name, max_multipart_uploads, cors, created_at)
		 VALUES (?, ?, ?, ?)`,
		b.Name, b.MaxMultipartUploads, cors, now(),
	)
}

// UpdateBucket writes what a bucket carries. The name identifies it and its
// objects, so it is the lookup rather than something this can change.
//
// created_at is left alone: a bucket whose limit changed is the same bucket.
func (s *Store) UpdateBucket(ctx context.Context, b *core.Bucket) error {
	cors, err := marshalCORS(b.CORS)
	if err != nil {
		return err
	}
	return s.exec(ctx, "update bucket "+b.Name,
		`UPDATE buckets SET max_multipart_uploads = ?, cors = ? WHERE name = ?`,
		b.MaxMultipartUploads, cors, b.Name,
	)
}

// CreateUser inserts a user.
func (s *Store) CreateUser(ctx context.Context, u *core.User) error {
	return s.exec(ctx, "create user "+u.Name,
		`INSERT INTO users (id, name, created_at) VALUES (?, ?, ?)`, u.ID, u.Name, now())
}

// CreateCredential inserts a keypair against an existing user.
//
// The error names the access key rather than the secret, which never reaches a
// log, an error string or an audit record on any path.
func (s *Store) CreateCredential(ctx context.Context, c *core.Credential) error {
	return s.exec(ctx, "create credential "+c.AccessKeyID,
		`INSERT INTO credentials
		   (access_key_id, user_id, secret, label, disabled, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		c.AccessKeyID, c.UserID, c.Secret, nullableString(c.Label), c.Disabled, now(),
	)
}

// CreateGrant gives a user access to one resource.
func (s *Store) CreateGrant(ctx context.Context, g *core.Grant) error {
	return s.exec(ctx, "create grant "+grantLabel(g.UserID, g.Resource),
		`INSERT INTO grants (user_id, resource_kind, resource_name, permissions, created_at) VALUES (?, ?, ?, ?, ?)`,
		grantArgs(g)...)
}

// RenameUser changes the name an operator reads a user by. The id is what
// credentials and grants reference and does not move.
func (s *Store) RenameUser(ctx context.Context, id, name string) error {
	return s.exec(ctx, "rename user "+id, `UPDATE users SET name = ? WHERE id = ?`, name, id)
}

// SetGrant records exactly what a user reaches on one resource, creating the
// grant when it is absent. created_at is left alone on the update, so a
// re-declared grant keeps its original age.
func (s *Store) SetGrant(ctx context.Context, g *core.Grant) error {
	return s.exec(ctx, "set grant "+grantLabel(g.UserID, g.Resource),
		`INSERT INTO grants (user_id, resource_kind, resource_name, permissions, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (user_id, resource_kind, resource_name)
		 DO UPDATE SET permissions = excluded.permissions`,
		grantArgs(g)...)
}

// grantArgs is the column list both grant writes insert, in schema order.
func grantArgs(g *core.Grant) []any {
	return []any{g.UserID, string(g.Resource.Kind), g.Resource.Name, g.Permissions.String(), now()}
}

// grantLabel names a grant in an error as "user -> kind name".
func grantLabel(userID string, r core.Resource) string {
	return userID + " -> " + string(r.Kind) + " " + r.Name
}

// DeleteBucket removes a stored bucket. Objects under it are unaffected, so a
// caller that means to destroy data does that first.
func (s *Store) DeleteBucket(ctx context.Context, name string) error {
	return s.exec(ctx, "delete bucket "+name, `DELETE FROM buckets WHERE name = ?`, name)
}

// DeleteUser removes a user. The schema refuses while it still holds credentials
// or grants, so those are removed first and revocation stays an explicit act.
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	return s.exec(ctx, "delete user "+id, `DELETE FROM users WHERE id = ?`, id)
}

// DeleteCredential revokes one keypair, leaving its siblings working.
func (s *Store) DeleteCredential(ctx context.Context, accessKeyID string) error {
	return s.exec(ctx, "delete credential "+accessKeyID, `DELETE FROM credentials WHERE access_key_id = ?`, accessKeyID)
}

// DeleteGrant withdraws a user's access to one resource, leaving its other
// grants in place.
func (s *Store) DeleteGrant(ctx context.Context, userID string, r core.Resource) error {
	return s.exec(ctx, "delete grant "+grantLabel(userID, r),
		`DELETE FROM grants WHERE user_id = ? AND resource_kind = ? AND resource_name = ?`,
		userID, string(r.Kind), r.Name)
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
		created  string
		lastUsed sql.NullString
	)
	if err := rows.Scan(&c.AccessKeyID, &c.UserID, &c.Secret,
		&label, &c.Disabled, &created, &lastUsed); err != nil {
		return core.Credential{}, fmt.Errorf("scan credential: %w", err)
	}
	c.Label = nullStringValue(label)

	var err error
	if c.CreatedAt, err = parseTime(created); err != nil {
		return core.Credential{}, fmt.Errorf("parse credential created_at: %w", err)
	}
	c.LastUsedAt = parseNullableTime(lastUsed)
	return c, nil
}

// scanGrant reads one grants row.
func scanGrant(rows *sql.Rows) (core.Grant, error) {
	var userID, kind, name, perms, created string
	if err := rows.Scan(&userID, &kind, &name, &perms, &created); err != nil {
		return core.Grant{}, fmt.Errorf("scan grant: %w", err)
	}
	createdAt, err := parseTime(created)
	if err != nil {
		return core.Grant{}, fmt.Errorf("parse grant created_at: %w", err)
	}
	return core.GrantFromColumns(userID, kind, name, perms, createdAt)
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// marshalCORS encodes a bucket's browser rules as text. The cors column is
// TEXT, and passing the encoded bytes would store a BLOB instead.
func marshalCORS(rules []config.CORSRule) (sql.NullString, error) {
	encoded, err := core.EncodeCORS(rules)
	if err != nil {
		return sql.NullString{}, err
	}
	return nullableString(string(encoded)), nil
}

// unmarshalCORS reads a bucket's browser rules back from the text column.
func unmarshalCORS(s sql.NullString) ([]config.CORSRule, error) {
	return core.DecodeCORS([]byte(nullStringValue(s)))
}
