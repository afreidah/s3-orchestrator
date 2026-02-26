-- name: EnqueueCleanup :exec
INSERT INTO cleanup_queue (backend_name, object_key, reason)
VALUES ($1, $2, $3);

-- name: GetPendingCleanups :many
SELECT id, backend_name, object_key, reason, attempts
FROM cleanup_queue
WHERE next_retry <= NOW() AND attempts < 10
ORDER BY created_at ASC
LIMIT $1;

-- name: DeleteCleanupItem :exec
DELETE FROM cleanup_queue WHERE id = $1;

-- name: UpdateCleanupRetry :exec
UPDATE cleanup_queue
SET attempts = attempts + 1,
    next_retry = NOW() + @backoff::interval,
    last_error = @last_error
WHERE id = @id;

-- name: CountPendingCleanups :one
SELECT COUNT(*) FROM cleanup_queue WHERE attempts < 10;
