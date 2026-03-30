-- ---------------------------------------------------------------------------
-- S3 Orchestrator — Consolidated SQLite Schema (v1)
--
-- Translates all 7 PostgreSQL migrations into a single idempotent schema.
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
    bytes_used   INTEGER NOT NULL DEFAULT 0,
    bytes_limit  INTEGER NOT NULL,
    orphan_bytes INTEGER NOT NULL DEFAULT 0,
    updated_at   TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
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
    created_at     TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (object_key, backend_name)
);

CREATE INDEX IF NOT EXISTS idx_object_locations_backend
    ON object_locations(backend_name);

CREATE INDEX IF NOT EXISTS idx_object_locations_key_pattern
    ON object_locations(object_key);

CREATE INDEX IF NOT EXISTS idx_object_locations_created
    ON object_locations(created_at);

CREATE INDEX IF NOT EXISTS idx_object_locations_key_created
    ON object_locations(object_key, created_at);

-- Track in-progress multipart uploads.
CREATE TABLE IF NOT EXISTS multipart_uploads (
    upload_id    TEXT PRIMARY KEY,
    object_key   TEXT NOT NULL,
    backend_name TEXT NOT NULL REFERENCES backend_quotas(backend_name),
    content_type TEXT,
    metadata     TEXT,
    created_at   TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
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
    last_error   TEXT
);

CREATE INDEX IF NOT EXISTS idx_cleanup_queue_next_retry
    ON cleanup_queue(next_retry) WHERE attempts < 10;

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

-- Stamp the schema version after all tables and indexes are created.
INSERT INTO schema_version (version) VALUES (1);
