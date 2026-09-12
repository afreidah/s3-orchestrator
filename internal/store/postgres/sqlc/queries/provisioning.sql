-- -----------------------------------------------------------------------------
-- Provisioning Queries
--
-- Author: Alex Freidah
--
-- sqlc-input definitions for the store half of the bucket registry: the
-- buckets, users, keypairs and grants that exist as rows rather than as config
-- entries. The listings are what registry assembly reads before merging with
-- what config declares; the rest is how each row comes into being and stops
-- being.
--
-- Every listing orders in SQL so both engines hand back the same sequence and a
-- caller comparing two assemblies compares content rather than ordering.
-- -----------------------------------------------------------------------------

-- name: ListBuckets :many
SELECT name, max_multipart_uploads, cors, created_at
FROM buckets
ORDER BY name;

-- name: ListUsers :many
SELECT id, name, created_at
FROM users
ORDER BY name;

-- name: ListCredentials :many
-- Disabled credentials are included: whether one authenticates is the
-- registry's decision, and hiding them here would also hide them from the
-- operator asking what exists.
SELECT access_key_id, user_id, secret, label, disabled, created_at, last_used_at
FROM credentials
ORDER BY access_key_id;

-- name: ListGrants :many
SELECT user_id, resource_kind, resource_name, permissions, created_at
FROM grants
ORDER BY user_id, resource_kind, resource_name;

-- name: CreateBucket :exec
INSERT INTO buckets (name, max_multipart_uploads, cors)
VALUES (@name, @max_multipart_uploads, @cors);

-- name: CreateUser :exec
INSERT INTO users (id, name)
VALUES (@id, @name);

-- name: CreateCredential :exec
INSERT INTO credentials (access_key_id, user_id, secret, label, disabled)
VALUES (@access_key_id, @user_id, @secret, @label, @disabled);

-- name: CreateGrant :exec
-- permissions is the comma-separated set the grant carries; empty means all of
-- them, which is what every grant written before permissions existed holds.
INSERT INTO grants (user_id, resource_kind, resource_name, permissions)
VALUES (@user_id, @resource_kind, @resource_name, @permissions);

-- name: DeleteBucket :exec
-- Objects stored under the bucket are untouched, so a caller that means to
-- destroy data does that first.
DELETE FROM buckets WHERE name = @name;

-- name: DeleteUser :exec
-- Refused by the foreign keys while the user still holds credentials or grants,
-- so those are removed first and revocation stays an explicit act.
DELETE FROM users WHERE id = @id;

-- name: DeleteCredential :exec
DELETE FROM credentials WHERE access_key_id = @access_key_id;

-- name: DeleteGrant :exec
DELETE FROM grants
WHERE user_id = @user_id AND resource_kind = @resource_kind AND resource_name = @resource_name;
