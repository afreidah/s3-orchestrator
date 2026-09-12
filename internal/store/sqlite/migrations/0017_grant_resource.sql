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
-- The key gains the kind so one user can hold a grant on a bucket and on a
-- backend that happen to share a name.
--
-- Rebuilt rather than altered: SQLite cannot change a primary key in place, so
-- the table is recreated and copied through. The copy names its columns rather
-- than selecting star, because a star would silently reorder if the old table
-- ever gains a column between this migration being written and being run.

CREATE TABLE grants_new (
    user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    resource_kind TEXT NOT NULL DEFAULT 'bucket',
    resource_name TEXT NOT NULL,
    permissions   TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (user_id, resource_kind, resource_name)
);

INSERT INTO grants_new (user_id, resource_kind, resource_name, permissions, created_at)
SELECT user_id, 'bucket', bucket_name, permissions, created_at FROM grants;

DROP TABLE grants;

ALTER TABLE grants_new RENAME TO grants;
