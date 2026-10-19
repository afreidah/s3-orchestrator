-- -----------------------------------------------------------------------------
-- Usage Flush Ids
--
-- Author: Alex Freidah
--
-- One row per shared usage batch applied to backend_usage and
-- backend_request_usage, written in the same transaction as its deltas. A batch
-- whose staged copy outlived its commit is recognised by its id and not applied
-- a second time. Rows older than the batches that can still be staged are
-- pruned by the flush itself.
-- -----------------------------------------------------------------------------

-- +goose Up
CREATE TABLE usage_flushes (
    flush_id   TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_usage_flushes_applied_at ON usage_flushes (applied_at);

-- +goose Down
DROP TABLE usage_flushes;
