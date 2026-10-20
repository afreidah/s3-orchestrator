-- -----------------------------------------------------------------------------
-- Notification Outbox Pending Index
--
-- Author: Alex Freidah
--
-- The notifier deletes an outbox row once its endpoint's max_retries are
-- spent, so every row left is still deliverable. The drain no longer filters
-- on attempts, and the pending index stops excluding rows past ten attempts,
-- which an endpoint allowed more retries than that left stuck. Built
-- CONCURRENTLY so a live outbox keeps taking writes.
-- -----------------------------------------------------------------------------

-- +goose Up
-- +goose NO TRANSACTION

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_notification_outbox_pending_all
    ON notification_outbox (next_retry);
DROP INDEX CONCURRENTLY IF EXISTS idx_notification_outbox_pending;
ALTER INDEX idx_notification_outbox_pending_all RENAME TO idx_notification_outbox_pending;

-- +goose Down
-- +goose NO TRANSACTION

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_notification_outbox_pending_partial
    ON notification_outbox (next_retry) WHERE attempts < 10;
DROP INDEX CONCURRENTLY IF EXISTS idx_notification_outbox_pending;
ALTER INDEX idx_notification_outbox_pending_partial RENAME TO idx_notification_outbox_pending;
