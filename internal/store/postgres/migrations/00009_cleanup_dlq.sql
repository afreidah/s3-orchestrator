-- +goose Up
-- -----------------------------------------------------------------------------
-- Cleanup Queue Dead-Letter
--
-- Holds cleanup_queue rows that exhausted their retry budget without ever
-- succeeding at the physical backend delete. The bytes referenced by these
-- rows still occupy the backend's quota (they are real, undeleted objects),
-- so orphan_bytes is NOT decremented when a row is moved here. Operators use
-- this table to find unrecoverable orphans, investigate the root cause, and
-- retry or write off each entry deliberately.
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS cleanup_dlq (
    id                BIGSERIAL PRIMARY KEY,
    original_id       BIGINT NOT NULL,
    backend_name      TEXT NOT NULL REFERENCES backend_quotas(backend_name),
    object_key        TEXT NOT NULL,
    reason            TEXT NOT NULL,
    size_bytes        BIGINT NOT NULL DEFAULT 0,
    attempts          INT NOT NULL,
    first_enqueued_at TIMESTAMPTZ NOT NULL,
    moved_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_error        TEXT
);

CREATE INDEX IF NOT EXISTS idx_cleanup_dlq_backend
    ON cleanup_dlq(backend_name);

-- +goose Down
DROP INDEX IF EXISTS idx_cleanup_dlq_backend;
DROP TABLE IF EXISTS cleanup_dlq;
