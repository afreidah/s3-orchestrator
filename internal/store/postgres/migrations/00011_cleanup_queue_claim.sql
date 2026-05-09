-- -----------------------------------------------------------------------------
-- Cleanup Queue Claim Pattern
--
-- Author: Alex Freidah
--
-- Adds per-row claim state to cleanup_queue so worker ticks across instances
-- (and across reconnects within an instance) cannot dispatch the same row to
-- two goroutines. Without this, a connection death that releases the cleanup
-- queue advisory lock mid-tick lets a second instance refetch rows that the
-- first instance is still processing; the duplicate run double-decrements
-- orphan_bytes and double-bills the backend DELETE API request.
--
-- claimed_at NULL means unclaimed and immediately eligible. A non-NULL value
-- older than the configured grace period is reclaimable, so a worker that
-- crashed mid-process does not leave the row stuck. claimed_by is a stable
-- instance identifier used for observability only; correctness depends solely
-- on claimed_at and the FOR UPDATE SKIP LOCKED claim query.
--
-- The original idx_cleanup_queue_next_retry partial index is replaced by an
-- index on (next_retry, created_at) so the claim query (ORDER BY created_at
-- under a next_retry filter) does not require a separate sort. CREATE INDEX
-- CONCURRENTLY plus the goose no-transaction annotation keeps the migration
-- online for populated tables.
-- -----------------------------------------------------------------------------

-- +goose Up
-- +goose NO TRANSACTION
ALTER TABLE cleanup_queue ADD COLUMN IF NOT EXISTS claimed_at TIMESTAMPTZ;
ALTER TABLE cleanup_queue ADD COLUMN IF NOT EXISTS claimed_by TEXT;

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_cleanup_queue_claim
    ON cleanup_queue(next_retry, created_at) WHERE attempts < 10;

DROP INDEX CONCURRENTLY IF EXISTS idx_cleanup_queue_next_retry;

-- +goose Down
-- +goose NO TRANSACTION
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_cleanup_queue_next_retry
    ON cleanup_queue(next_retry) WHERE attempts < 10;

DROP INDEX CONCURRENTLY IF EXISTS idx_cleanup_queue_claim;

ALTER TABLE cleanup_queue DROP COLUMN IF EXISTS claimed_by;
ALTER TABLE cleanup_queue DROP COLUMN IF EXISTS claimed_at;
