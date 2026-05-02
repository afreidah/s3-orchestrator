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
