-- The keys whose copies plus live intents are off the replication factor, so
-- the drain works through them instead of scanning the ledger to find them.
-- direction is -1 for a key short of the factor and 1 for one past it. Every
-- queue write bumps version, and the drain settles or retries a key only at the
-- version it read.

CREATE TABLE IF NOT EXISTS replication_queue (
    object_key  TEXT PRIMARY KEY,
    direction   INTEGER NOT NULL,
    attempts    INT NOT NULL DEFAULT 0,
    next_retry  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    version     INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_replication_queue_due
    ON replication_queue (direction, next_retry);
