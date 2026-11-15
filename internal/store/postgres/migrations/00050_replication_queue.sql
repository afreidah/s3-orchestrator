-- -----------------------------------------------------------------------------
-- Replication Queue
--
-- Author: Alex Freidah
--
-- The keys whose copies plus live intents are off the replication factor, so
-- the drain works through them instead of scanning the ledger to find them. A
-- write that leaves a key off factor queues it in its own transaction, and a
-- rebuild queues every one the ledger holds. direction is -1 for a key short of
-- the factor and 1 for one past it. Every queue write bumps version, and the
-- drain settles or retries a key only at the version it read.
-- -----------------------------------------------------------------------------

-- +goose Up

CREATE TABLE IF NOT EXISTS replication_queue (
    object_key  TEXT PRIMARY KEY,
    direction   SMALLINT NOT NULL,
    attempts    INT NOT NULL DEFAULT 0,
    next_retry  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    version     BIGINT NOT NULL DEFAULT 1,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_replication_queue_due
    ON replication_queue (direction, next_retry);

-- +goose Down

DROP TABLE IF EXISTS replication_queue;
