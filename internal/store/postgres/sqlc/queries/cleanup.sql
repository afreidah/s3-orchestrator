-- -----------------------------------------------------------------------------
-- Cleanup Queue Queries
--
-- Author: Alex Freidah
--
-- sqlc-input definitions for cleanup_queue and cleanup_dlq operations.
-- Covers the enqueue + retry-state lifecycle, the move-to-DLQ flow that
-- graduates exhausted rows, and the engine-side helpers core/ uses for the
-- atomic stale-row sweep that keeps orphan_bytes in lockstep with row
-- delete.
-- -----------------------------------------------------------------------------

-- name: EnqueueCleanup :exec
INSERT INTO cleanup_queue (backend_name, object_key, reason, size_bytes)
VALUES ($1, $2, $3, $4);

-- name: GetPendingCleanups :many
-- Read-only listing for the admin endpoint and operator visibility. The
-- worker uses ClaimPendingCleanups instead, which also stamps the row.
-- claimed_at and claimed_by are projected so the admin view can render
-- which rows are currently held by a worker.
SELECT id, backend_name, object_key, reason, attempts, size_bytes,
       claimed_at, claimed_by
FROM cleanup_queue
WHERE next_retry <= NOW() AND attempts < 10
ORDER BY created_at ASC
LIMIT $1;

-- name: ClaimPendingCleanups :many
-- Atomically claims the next batch of cleanup rows for the calling
-- instance. The inner SELECT uses FOR UPDATE SKIP LOCKED so two
-- concurrent claim transactions across instances return disjoint row
-- sets. A row is eligible if it has never been claimed (claimed_at IS
-- NULL) or its claim has aged past the grace cutoff (claimed_at <
-- @grace_cutoff). The reclaimed projection lets the worker increment
-- s3o_cleanup_queue_stale_claims_recovered_total per recovered row.
WITH candidate AS (
    SELECT cq.id, (cq.claimed_at IS NOT NULL)::boolean AS reclaimed
    FROM cleanup_queue cq
    WHERE cq.next_retry <= NOW()
      AND cq.attempts < 10
      AND (cq.claimed_at IS NULL OR cq.claimed_at < @grace_cutoff::timestamptz)
    ORDER BY cq.created_at ASC
    LIMIT $1
    FOR UPDATE SKIP LOCKED
),
claimed AS (
    UPDATE cleanup_queue cq
    SET claimed_at = NOW(),
        claimed_by = @claimed_by::text
    FROM candidate c
    WHERE cq.id = c.id
    RETURNING cq.id, cq.backend_name, cq.object_key, cq.reason, cq.attempts, cq.size_bytes
)
SELECT cl.id, cl.backend_name, cl.object_key, cl.reason, cl.attempts, cl.size_bytes,
       c.reclaimed
FROM claimed cl
JOIN candidate c ON cl.id = c.id
ORDER BY cl.id;

-- name: DeleteCleanupItem :exec
-- Used inside the move-to-DLQ transaction; orphan_bytes is intentionally
-- left untouched because the underlying backend object is still on disk.
-- The cleanup worker's success path uses CompleteCleanupItem instead,
-- which atomically deletes the row and decrements orphan_bytes.
DELETE FROM cleanup_queue WHERE id = $1;

-- name: CompleteCleanupItem :exec
-- Atomically removes a successfully-processed cleanup_queue row and
-- decrements the backing backend's orphan_bytes by the row's size.
-- Idempotent against re-claim retries: if the row was already deleted
-- by a previous worker, the CTE is empty and orphan_bytes is not
-- double-decremented. GREATEST(0, ...) clamps the counter so a stale
-- size_bytes value cannot drive orphan_bytes negative.
WITH deleted AS (
    DELETE FROM cleanup_queue WHERE id = $1
    RETURNING backend_name, size_bytes
)
UPDATE backend_quotas bq
SET orphan_bytes = GREATEST(0, orphan_bytes - d.size_bytes)
FROM deleted d
WHERE bq.backend_name = d.backend_name;

-- name: UpdateCleanupRetry :exec
-- Schedules a retry by advancing next_retry, recording the error, and
-- clearing the claim so the row is immediately re-eligible for the next
-- worker tick (the previous claim is what gated this retry from running
-- twice within the grace window).
UPDATE cleanup_queue
SET attempts = attempts + 1,
    next_retry = NOW() + @backoff::interval,
    last_error = @last_error,
    claimed_at = NULL,
    claimed_by = NULL
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

-- name: HasPendingCleanup :one
-- Reports whether a delete for (object_key, backend_name) is still
-- outstanding: either waiting in the retry queue or dead-lettered after
-- exhausting its attempts. Both mean the bytes are still on the backend and
-- the object is meant to be gone, which is what stops reconcile importing it
-- back. Dead-lettered counts because retrying stopped, not because the delete
-- was withdrawn.
SELECT EXISTS (
    SELECT 1 FROM cleanup_queue q
     WHERE q.object_key = @object_key AND q.backend_name = @backend_name
    UNION ALL
    SELECT 1 FROM cleanup_dlq d
     WHERE d.object_key = @object_key AND d.backend_name = @backend_name
) AS pending;

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

-- name: ListCleanupDLQ :many
-- Read-only listing of dead-lettered cleanups for the admin cleanup-dlq
-- view. An empty backend argument returns every backend; otherwise the
-- listing is scoped to one. Newest graduations first.
SELECT backend_name, object_key, reason, size_bytes,
       attempts, first_enqueued_at, moved_at, last_error
FROM cleanup_dlq
WHERE (sqlc.arg(backend)::text = '' OR backend_name = sqlc.arg(backend))
ORDER BY moved_at DESC
LIMIT sqlc.arg(row_limit);

-- name: RequeueCleanupDLQ :execrows
-- Atomically moves dead-lettered rows back into cleanup_queue so the
-- cleanup worker retries them against a now-recovered backend. The
-- writable CTE deletes the DLQ rows and re-inserts them in one
-- statement; cleanup_queue defaults reset created_at/next_retry to NOW()
-- and attempts to 0. orphan_bytes is untouched here - it was never
-- decremented on the DLQ move, and the worker will debit it when the
-- retried delete finally succeeds. An empty backend requeues every
-- backend. Returns the number of rows requeued.
WITH moved AS (
    DELETE FROM cleanup_dlq
    WHERE (sqlc.arg(backend)::text = '' OR backend_name = sqlc.arg(backend))
    RETURNING backend_name, object_key, reason, size_bytes
)
INSERT INTO cleanup_queue (backend_name, object_key, reason, size_bytes)
SELECT backend_name, object_key, reason, size_bytes FROM moved;
