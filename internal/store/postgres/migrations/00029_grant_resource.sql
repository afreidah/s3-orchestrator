-- -----------------------------------------------------------------------------
-- Grant Resource
--
-- Author: Alex Freidah
--
-- Lets a grant name something other than a bucket. The control plane has no
-- bucket to hang a grant off: draining a backend, rotating a key and reading
-- status are not operations on one, so an admin action set has nothing to be
-- granted on until a grant can name a resource.
--
-- A resource is a kind and a name. The kinds are bucket, backend and fleet;
-- fleet carries the empty name, because there is only one of it and the name is
-- part of the key rather than nullable.
--
-- Every existing row is a bucket grant and keeps meaning exactly what it meant.
-- The column is renamed rather than replaced, so no row is rewritten and none
-- can be missed. The key gains the kind so one user can hold a grant on a bucket
-- and on a backend that happen to share a name.
--
-- The down path discards every grant that is not a bucket grant, because the
-- old shape cannot express one. That is data loss on downgrade, and it is the
-- reason to take a backup before rolling back rather than after.
-- -----------------------------------------------------------------------------

-- +goose Up
ALTER TABLE grants RENAME COLUMN bucket_name TO resource_name;
ALTER TABLE grants ADD COLUMN IF NOT EXISTS resource_kind TEXT NOT NULL DEFAULT 'bucket';

ALTER TABLE grants DROP CONSTRAINT grants_pkey;
ALTER TABLE grants ADD PRIMARY KEY (user_id, resource_kind, resource_name);

-- +goose Down
DELETE FROM grants WHERE resource_kind <> 'bucket';
ALTER TABLE grants DROP CONSTRAINT grants_pkey;
ALTER TABLE grants DROP COLUMN IF EXISTS resource_kind;
ALTER TABLE grants RENAME COLUMN resource_name TO bucket_name;
ALTER TABLE grants ADD PRIMARY KEY (user_id, bucket_name);
