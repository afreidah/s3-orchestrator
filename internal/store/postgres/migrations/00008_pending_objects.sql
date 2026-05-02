-- -------------------------------------------------------------------------------
-- Pending Objects - In-Flight PUT Intent Tracking
--
-- Author: Alex Freidah
--
-- Tracks the intent of an in-flight PutObject so that a metadata commit
-- failure (e.g. transient DB outage) cannot silently destroy the prior copy
-- of an overwritten key. The write path inserts an intent row before
-- uploading bytes to a backend; on a successful commit the row is deleted in
-- the same transaction. On commit failure the row remains, and the pending
-- reaper resolves it on a later tick by HEADing the backend: bytes present
-- promote the row into object_locations, bytes absent simply remove the row.
-- -------------------------------------------------------------------------------

-- +goose Up

CREATE TABLE IF NOT EXISTS pending_objects (
    intent_id      TEXT PRIMARY KEY,
    object_key     TEXT NOT NULL,
    backend_name   TEXT NOT NULL REFERENCES backend_quotas(backend_name),
    size_bytes     BIGINT NOT NULL,
    encrypted      BOOLEAN NOT NULL DEFAULT FALSE,
    encryption_key BYTEA,
    key_id         TEXT,
    plaintext_size BIGINT,
    content_hash   TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_pending_objects_created
    ON pending_objects(created_at);

CREATE INDEX IF NOT EXISTS idx_pending_objects_backend
    ON pending_objects(backend_name);

-- +goose Down
DROP TABLE IF EXISTS pending_objects;
