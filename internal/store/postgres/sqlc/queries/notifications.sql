-- -----------------------------------------------------------------------------
-- Notification Outbox Queries
--
-- Author: Alex Freidah
--
-- sqlc-input definitions for notification_outbox. Covers the durable webhook
-- delivery flow: synchronous insert at every event site, fetch-pending with
-- backoff-aware scheduling, complete on successful delivery, and retry with
-- exponential backoff on failure.
-- -----------------------------------------------------------------------------

-- name: InsertNotification :exec
INSERT INTO notification_outbox (event_type, payload, endpoint_url)
VALUES ($1, $2, $3);

-- name: GetPendingNotifications :many
SELECT id, event_type, payload, endpoint_url, attempts
FROM notification_outbox
WHERE next_retry <= NOW() AND attempts < 10
ORDER BY created_at ASC
LIMIT $1;

-- name: CompleteNotification :exec
DELETE FROM notification_outbox WHERE id = $1;

-- name: RetryNotification :exec
UPDATE notification_outbox
SET attempts = attempts + 1,
    next_retry = NOW() + @backoff::interval,
    last_error = @last_error
WHERE id = @id;
