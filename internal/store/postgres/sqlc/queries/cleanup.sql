-- name: EnqueueCleanup :exec
INSERT INTO cleanup_queue (backend_name, object_key, reason, size_bytes)
VALUES ($1, $2, $3, $4);

-- name: GetPendingCleanups :many
SELECT id, backend_name, object_key, reason, attempts, size_bytes
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

-- name: DeleteCleanupQueueByBackend :exec
DELETE FROM cleanup_queue WHERE backend_name = $1;

-- name: SumCleanupQueueSizeByKey :one
-- Returns the sum of size_bytes for every cleanup_queue row matching the
-- given (object_key, backend_name) pair. Used by the reconciler-driven
-- sweep so orphan_bytes can be decremented in step with the row delete.
SELECT COALESCE(SUM(size_bytes), 0)::bigint AS total_bytes,
       COUNT(*)::bigint AS row_count
FROM cleanup_queue
WHERE object_key = $1 AND backend_name = $2;

-- name: DeleteCleanupQueueByKey :execrows
-- Removes every cleanup_queue row matching the given (object_key,
-- backend_name) pair. Returns the number of rows deleted so the caller
-- can confirm the sum-then-delete pair stayed consistent.
DELETE FROM cleanup_queue
WHERE object_key = $1 AND backend_name = $2;

-- name: GetCleanupQueueRow :one
-- Fetches a single cleanup_queue row by id along with the columns the
-- DLQ insert needs (backend, key, reason, size, attempts, created_at,
-- last_error). Used inside MoveCleanupToDLQ so the row contents survive
-- the queue->DLQ move.
SELECT id, backend_name, object_key, reason, size_bytes,
       attempts, created_at, last_error
FROM cleanup_queue
WHERE id = $1;

-- name: InsertCleanupDLQ :exec
-- Inserts an exhausted cleanup_queue row into the dead-letter table.
-- The original_id retains the queue row's id for forensic correlation;
-- first_enqueued_at carries the original created_at so the DLQ entry
-- remembers how long the cleanup was outstanding.
INSERT INTO cleanup_dlq (
    original_id, backend_name, object_key, reason, size_bytes,
    attempts, first_enqueued_at, last_error
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: CountCleanupDLQ :one
-- Returns the current depth of the cleanup_dlq table for the dashboard
-- and the cleanup_dlq_depth gauge.
SELECT COUNT(*) FROM cleanup_dlq;
