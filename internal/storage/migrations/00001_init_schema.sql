-- -------------------------------------------------------------------------------
-- Initial Schema - S3 Orchestrator Database
--
-- Author: Alex Freidah
--
-- PostgreSQL schema for quota tracking, object location storage, multipart
-- uploads, usage tracking, and cleanup queue. IF NOT EXISTS guards ensure safe
-- application on databases with pre-existing schema.
-- -------------------------------------------------------------------------------

-- +goose Up

-- Track quota usage per backend
CREATE TABLE IF NOT EXISTS backend_quotas (
    backend_name TEXT PRIMARY KEY,
    bytes_used   BIGINT NOT NULL DEFAULT 0,
    bytes_limit  BIGINT NOT NULL,
    updated_at   TIMESTAMPTZ DEFAULT NOW()
);

-- Track which backend stores which object (composite PK supports replication)
CREATE TABLE IF NOT EXISTS object_locations (
    object_key   TEXT NOT NULL,
    backend_name TEXT NOT NULL REFERENCES backend_quotas(backend_name),
    size_bytes   BIGINT NOT NULL,
    created_at   TIMESTAMPTZ DEFAULT NOW(),
    PRIMARY KEY (object_key, backend_name)
);

-- Index for efficient backend lookups
CREATE INDEX IF NOT EXISTS idx_object_locations_backend
    ON object_locations(backend_name);

-- Index for efficient LIKE prefix queries on object keys
CREATE INDEX IF NOT EXISTS idx_object_locations_key_pattern
    ON object_locations(object_key text_pattern_ops);

-- Index for cleanup queries
CREATE INDEX IF NOT EXISTS idx_object_locations_created
    ON object_locations(created_at);

-- Track in-progress multipart uploads
CREATE TABLE IF NOT EXISTS multipart_uploads (
    upload_id    TEXT PRIMARY KEY,
    object_key   TEXT NOT NULL,
    backend_name TEXT NOT NULL REFERENCES backend_quotas(backend_name),
    content_type TEXT,
    created_at   TIMESTAMPTZ DEFAULT NOW()
);

-- Track individual parts of multipart uploads
CREATE TABLE IF NOT EXISTS multipart_parts (
    upload_id   TEXT NOT NULL REFERENCES multipart_uploads(upload_id) ON DELETE CASCADE,
    part_number INT NOT NULL,
    etag        TEXT NOT NULL,
    size_bytes  BIGINT NOT NULL,
    created_at  TIMESTAMPTZ DEFAULT NOW(),
    PRIMARY KEY (upload_id, part_number)
);

-- Index for cleaning up stale multipart uploads
CREATE INDEX IF NOT EXISTS idx_multipart_uploads_created
    ON multipart_uploads(created_at);

-- Index for efficient LIKE prefix queries on multipart upload keys
CREATE INDEX IF NOT EXISTS idx_multipart_uploads_key_pattern
    ON multipart_uploads(object_key text_pattern_ops);

-- Track per-backend API requests and data transfer by month
CREATE TABLE IF NOT EXISTS backend_usage (
    backend_name  TEXT NOT NULL REFERENCES backend_quotas(backend_name),
    period        TEXT NOT NULL,
    api_requests  BIGINT NOT NULL DEFAULT 0,
    egress_bytes  BIGINT NOT NULL DEFAULT 0,
    ingress_bytes BIGINT NOT NULL DEFAULT 0,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (backend_name, period)
);

-- Queue for retrying failed backend object deletions (orphan cleanup)
CREATE TABLE IF NOT EXISTS cleanup_queue (
    id           BIGSERIAL PRIMARY KEY,
    backend_name TEXT NOT NULL REFERENCES backend_quotas(backend_name),
    object_key   TEXT NOT NULL,
    reason       TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    next_retry   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    attempts     INT NOT NULL DEFAULT 0,
    last_error   TEXT
);

CREATE INDEX IF NOT EXISTS idx_cleanup_queue_next_retry
    ON cleanup_queue(next_retry) WHERE attempts < 10;

-- +goose Down

DROP TABLE IF EXISTS cleanup_queue;
DROP TABLE IF EXISTS backend_usage;
DROP TABLE IF EXISTS multipart_parts;
DROP TABLE IF EXISTS multipart_uploads;
DROP TABLE IF EXISTS object_locations;
DROP TABLE IF EXISTS backend_quotas;
