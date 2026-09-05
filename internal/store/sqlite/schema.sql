-- ---------------------------------------------------------------------------
-- S3 Orchestrator — Consolidated SQLite Schema (v1)
--
-- Translates the PostgreSQL migrations into a single idempotent schema.
-- Translation rules applied:
--   BIGSERIAL        → INTEGER PRIMARY KEY AUTOINCREMENT  (id columns only)
--   TIMESTAMPTZ      → TEXT  (ISO-8601 strings)
--   DEFAULT NOW()    → DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
--   JSONB            → TEXT
--   BYTEA            → BLOB
--   BIGINT           → INTEGER
--   BOOLEAN          → INTEGER  (0/1)
--   text_pattern_ops → removed (SQLite has no operator classes)
--   CONCURRENTLY     → removed (SQLite has no concurrent DDL)
-- ---------------------------------------------------------------------------

-- Schema version tracking (SQLite-specific, replaces goose_db_version).
CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER NOT NULL
);

-- Track quota usage per backend.
CREATE TABLE IF NOT EXISTS backend_quotas (
    backend_name TEXT PRIMARY KEY,
    bytes_limit  INTEGER NOT NULL,
    orphan_bytes INTEGER NOT NULL DEFAULT 0,
    updated_at   TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- A backend's stored byte total, split across stripe rows so concurrent writers
-- take different row locks. The total is the sum; an individual stripe is
-- signed and carries no meaning on its own.
CREATE TABLE IF NOT EXISTS backend_quota_stripes (
    backend_name TEXT    NOT NULL REFERENCES backend_quotas(backend_name) ON DELETE CASCADE,
    stripe_id    INTEGER NOT NULL,
    bytes_used   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (backend_name, stripe_id)
);

-- Track which backend stores which object (composite PK supports replication).
CREATE TABLE IF NOT EXISTS object_locations (
    object_key     TEXT NOT NULL,
    backend_name   TEXT NOT NULL REFERENCES backend_quotas(backend_name),
    size_bytes     INTEGER NOT NULL,
    encrypted      INTEGER NOT NULL DEFAULT 0,
    encryption_key BLOB,
    key_id         TEXT,
    plaintext_size INTEGER,
    content_hash   TEXT,
    managed        INTEGER NOT NULL DEFAULT 1,
    -- NULL means never verified; the scrub queue falls back to created_at.
    last_scrubbed_at TEXT,
    -- NULL algorithm means the bytes are stored verbatim. logical_size is the
    -- size the client wrote, which differs from plaintext_size once the stored
    -- bytes are ciphertext of compressed data.
    compression_algorithm      TEXT,
    compression_level          TEXT,
    compression_format_version INTEGER,
    logical_size               INTEGER,
    -- What the encoder produced for a copy it declined to store compressed, and
    -- the level it produced it at. NULL means never probed. The uncompressed
    -- listing judges these against the current settings so a copy already known
    -- not to shrink enough is not downloaded and encoded again to find out.
    compression_probe_size     INTEGER,
    compression_probe_level    TEXT,
    -- What a HEAD answers with, held here so every copy reports the same
    -- validator and the backend round trip can be skipped. NULL means unknown,
    -- which is distinct from a known-empty content type or metadata set.
    etag           TEXT,
    content_type   TEXT,
    user_metadata  TEXT,
    created_at     TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (object_key, backend_name)
);

CREATE INDEX IF NOT EXISTS idx_object_locations_backend
    ON object_locations(backend_name);

CREATE INDEX IF NOT EXISTS idx_object_locations_key_pattern
    ON object_locations(object_key);

CREATE INDEX IF NOT EXISTS idx_object_locations_created
    ON object_locations(created_at);

-- Backs the scrub queue: least recently touched first, so a freshly written
-- copy sorts behind an old one that has gone unverified.
CREATE INDEX IF NOT EXISTS idx_object_locations_scrub_queue
    ON object_locations(COALESCE(last_scrubbed_at, created_at), object_key);

CREATE INDEX IF NOT EXISTS idx_object_locations_key_created
    ON object_locations(object_key, created_at);

CREATE INDEX IF NOT EXISTS idx_object_locations_managed
    ON object_locations(backend_name) WHERE managed;

-- Track in-progress multipart uploads.
CREATE TABLE IF NOT EXISTS multipart_uploads (
    upload_id      TEXT PRIMARY KEY,
    object_key     TEXT NOT NULL,
    backend_name   TEXT NOT NULL REFERENCES backend_quotas(backend_name),
    content_type   TEXT,
    metadata       TEXT,
    encryption_key BLOB,
    key_id         TEXT,
    tagging        TEXT,
    created_at     TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_multipart_uploads_created
    ON multipart_uploads(created_at);

CREATE INDEX IF NOT EXISTS idx_multipart_uploads_key_pattern
    ON multipart_uploads(object_key);

CREATE INDEX IF NOT EXISTS idx_multipart_uploads_backend_name
    ON multipart_uploads(backend_name);

-- Track individual parts of multipart uploads.
CREATE TABLE IF NOT EXISTS multipart_parts (
    upload_id      TEXT NOT NULL REFERENCES multipart_uploads(upload_id) ON DELETE CASCADE,
    part_number    INT NOT NULL,
    etag           TEXT NOT NULL,
    size_bytes     INTEGER NOT NULL,
    encrypted      INTEGER NOT NULL DEFAULT 0,
    encryption_key BLOB,
    key_id         TEXT,
    plaintext_size INTEGER,
    -- MD5 of the bytes the client sent for this part, which etag is not once
    -- the stored part is ciphertext. The AWS multipart ETag is the MD5 of the
    -- concatenated part digests, so it can only be built from these.
    plaintext_etag TEXT,
    created_at     TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (upload_id, part_number)
);

-- Track per-backend API requests and data transfer by month.
CREATE TABLE IF NOT EXISTS backend_usage (
    backend_name  TEXT NOT NULL REFERENCES backend_quotas(backend_name),
    period        TEXT NOT NULL,
    api_requests  INTEGER NOT NULL DEFAULT 0,
    egress_bytes  INTEGER NOT NULL DEFAULT 0,
    ingress_bytes INTEGER NOT NULL DEFAULT 0,
    updated_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (backend_name, period)
);

-- Per-pool request counts, keyed by the pool names config declares. Additive
-- with each other rather than a decomposition of backend_usage.api_requests:
-- an operation charges every pool that contains it.
CREATE TABLE IF NOT EXISTS backend_request_usage (
    backend_name TEXT NOT NULL REFERENCES backend_quotas(backend_name),
    period       TEXT NOT NULL,
    pool         TEXT NOT NULL,
    requests     INTEGER NOT NULL DEFAULT 0,
    updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (backend_name, period, pool)
);

CREATE INDEX IF NOT EXISTS idx_backend_request_usage_period
    ON backend_request_usage(period);

-- Queue for retrying failed backend object deletions (orphan cleanup).
CREATE TABLE IF NOT EXISTS cleanup_queue (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    backend_name TEXT NOT NULL REFERENCES backend_quotas(backend_name),
    object_key   TEXT NOT NULL,
    reason       TEXT NOT NULL,
    size_bytes   INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    next_retry   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    attempts     INT NOT NULL DEFAULT 0,
    last_error   TEXT,
    claimed_at   TEXT,
    claimed_by   TEXT
);

CREATE INDEX IF NOT EXISTS idx_cleanup_queue_claim
    ON cleanup_queue(next_retry, created_at) WHERE attempts < 10;

-- Dead-letter for cleanup_queue rows that exhausted their retry budget
-- without ever succeeding at the physical backend delete. The bytes are
-- still on disk, so orphan_bytes is NOT decremented; operators inspect
-- this table to find unrecoverable orphans and decide whether to retry
-- or write each entry off deliberately.
CREATE TABLE IF NOT EXISTS cleanup_dlq (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    original_id       INTEGER NOT NULL,
    backend_name      TEXT NOT NULL REFERENCES backend_quotas(backend_name),
    object_key        TEXT NOT NULL,
    reason            TEXT NOT NULL,
    size_bytes        INTEGER NOT NULL DEFAULT 0,
    attempts          INT NOT NULL,
    first_enqueued_at TEXT NOT NULL,
    moved_at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    last_error        TEXT
);

CREATE INDEX IF NOT EXISTS idx_cleanup_dlq_backend
    ON cleanup_dlq(backend_name);

-- Durable webhook notification delivery queue.
CREATE TABLE IF NOT EXISTS notification_outbox (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    event_type   TEXT NOT NULL,
    payload      TEXT NOT NULL,
    endpoint_url TEXT NOT NULL,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    next_retry   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    attempts     INT NOT NULL DEFAULT 0,
    last_error   TEXT
);

CREATE INDEX IF NOT EXISTS idx_notification_outbox_pending
    ON notification_outbox(next_retry) WHERE attempts < 10;

-- In-flight PutObject intent tracking. The write path inserts a row before
-- the backend PUT and removes it on a successful metadata commit; the
-- pending reaper resolves any rows left behind by a failed commit so a DB
-- outage between PUT and RecordObject cannot silently destroy the prior
-- copy of an overwritten key. See migrations/00008_pending_objects.sql for
-- the full design notes (mirrored here for the SQLite backend).
CREATE TABLE IF NOT EXISTS pending_objects (
    intent_id      TEXT PRIMARY KEY,
    object_key     TEXT NOT NULL,
    backend_name   TEXT NOT NULL REFERENCES backend_quotas(backend_name),
    size_bytes     INTEGER NOT NULL,
    encrypted      INTEGER NOT NULL DEFAULT 0,
    encryption_key BLOB,
    key_id         TEXT,
    plaintext_size INTEGER,
    content_hash   TEXT,
    compression_algorithm      TEXT,
    compression_level          TEXT,
    compression_format_version INTEGER,
    logical_size               INTEGER,
    -- Carried so a reaper-promoted intent keeps the identity the write knew.
    etag           TEXT,
    content_type   TEXT,
    user_metadata  TEXT,
    created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_pending_objects_created
    ON pending_objects(created_at);

CREATE INDEX IF NOT EXISTS idx_pending_objects_backend
    ON pending_objects(backend_name);

-- S3 object tags, keyed by object rather than by copy so replicas of a key
-- cannot disagree about the set. Rows rather than a JSON column because
-- lifecycle expiry by tag filters on (tag_key, tag_value) and needs an index.
-- No foreign key: nothing is keyed on object_key alone, so core clears these
-- rows at every path that puts a new object at a key or removes its last copy.
-- See migrations/0009_object_tags.sql for the full design notes.
CREATE TABLE IF NOT EXISTS object_tags (
    object_key TEXT NOT NULL,
    tag_key    TEXT NOT NULL,
    tag_value  TEXT NOT NULL,
    PRIMARY KEY (object_key, tag_key)
);

CREATE INDEX IF NOT EXISTS idx_object_tags_lookup
    ON object_tags(tag_key, tag_value);

-- Stamp the schema version after all tables and indexes are created.
INSERT INTO schema_version (version) VALUES (13);
