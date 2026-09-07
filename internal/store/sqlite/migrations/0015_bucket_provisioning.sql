-- A database-backed set of buckets and the users that reach them, merged with
-- what the config file declares when the bucket registry is assembled. Config is
-- authoritative for any name it carries.
--
-- Resolution runs the chain: an access key names a credential, a credential
-- belongs to a user, and a user holds a grant per bucket it may reach.
-- Permissions attach to the user, so a rotated credential changes nothing about
-- what its holder can do.
--
-- A grant carries no permission column, and its absence means full access to the
-- named bucket. Nothing limits a user to one grant.
--
-- grants.bucket_name has no foreign key: a bucket declared in config has no row
-- here, and a user may hold a grant on one. A grant naming a bucket that exists
-- in neither source is reported when the registry is assembled.
--
-- The secret is stored as written, because SigV4 sends a signature rather than
-- the secret and the server repeats the client's key derivation to verify it.

CREATE TABLE IF NOT EXISTS buckets (
    name                  TEXT PRIMARY KEY,
    max_multipart_uploads INTEGER NOT NULL DEFAULT 0,
    cors                  TEXT,
    created_at            TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE IF NOT EXISTS users (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- Removing a user that still holds credentials or grants is refused, so
-- revocation is an explicit act rather than a side effect.
CREATE TABLE IF NOT EXISTS credentials (
    access_key_id TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    secret        TEXT NOT NULL,
    label         TEXT,
    disabled      INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    last_used_at  TEXT
);

-- Assembly reads every credential a user holds, and revoking one leaves its
-- siblings, so the table is read by user as well as by access key.
CREATE INDEX IF NOT EXISTS idx_credentials_user
    ON credentials(user_id);

CREATE TABLE IF NOT EXISTS grants (
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    bucket_name TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (user_id, bucket_name)
);
