-- -----------------------------------------------------------------------------
-- Cleanup Queue Queries
--
-- Author: Alex Freidah
--
-- sqlc-input definitions for cleanup_queue and cleanup_dlq operations: the
-- insert every copy-removing transaction makes, the claim and retry lifecycle,
-- and the move-to-DLQ flow that graduates exhausted rows. A backend's orphan
-- bytes are the sum of its rows here, read through backend_capacity.
-- -----------------------------------------------------------------------------

-- name: InsertCleanup :one
-- Queued inside the transaction that stopped recording the bytes, already
-- claimed by the instance that will delete them once it commits. A claim that
-- instance never settles becomes reclaimable after the grace period.
-- storage_key is what is deleted; object_key rides along so an operator reading
-- the queue still sees which object the bytes belonged to.
INSERT INTO cleanup_queue (backend_name, object_key, storage_key, reason, size_bytes, claimed_at, claimed_by)
VALUES (@backend_name, @object_key, @storage_key, @reason, @size_bytes, NOW(), @claimed_by)
RETURNING id, claimed_at;

-- name: GetPendingCleanups :many
-- Read-only listing for the admin endpoint and operator visibility. The
-- worker uses ClaimPendingCleanups instead, which also stamps the row.
-- claimed_at and claimed_by are projected so the admin view can render
-- which rows are currently held by a worker. Ordered like the claim, so the
-- listing shows rows in the order the worker will take them. Selects the same
-- columns as ClaimPendingCleanups so both map through one converter.
SELECT id, backend_name, object_key, storage_key, reason, attempts, size_bytes,
       claimed_at, claimed_by, FALSE::boolean AS reclaimed
FROM cleanup_queue
WHERE next_retry <= NOW() AND attempts < @max_attempts::int
ORDER BY next_retry ASC, created_at ASC
LIMIT @row_limit;

-- name: ClaimPendingCleanups :many
-- Atomically claims the next batch of cleanup rows for the calling
-- instance. The inner SELECT uses FOR UPDATE SKIP LOCKED so two
-- concurrent claim transactions across instances return disjoint row
-- sets. A row is eligible if it has never been claimed (claimed_at IS
-- NULL) or its claim has aged past the grace cutoff (claimed_at <
-- @grace_cutoff). The reclaimed projection lets the worker increment
-- s3o_cleanup_queue_stale_claims_recovered_total per recovered row, and the
-- claim it stamped comes back so every settle write can match it.
--
-- The order matches idx_cleanup_queue_claim, so the scan stops at the limit
-- instead of sorting every due row. A new row's next_retry is its enqueue
-- time, so first attempts still run oldest first.
WITH candidate AS (
    SELECT cq.id, (cq.claimed_at IS NOT NULL)::boolean AS reclaimed
    FROM cleanup_queue cq
    WHERE cq.next_retry <= NOW()
      AND cq.attempts < @max_attempts::int
      AND (cq.claimed_at IS NULL OR cq.claimed_at < @grace_cutoff::timestamptz)
    ORDER BY cq.next_retry ASC, cq.created_at ASC
    LIMIT @row_limit
    FOR UPDATE SKIP LOCKED
),
claimed AS (
    UPDATE cleanup_queue cq
    SET claimed_at = NOW(),
        claimed_by = @claimed_by::text
    FROM candidate c
    WHERE cq.id = c.id
    RETURNING cq.id, cq.backend_name, cq.object_key, cq.storage_key, cq.reason, cq.attempts, cq.size_bytes,
              cq.claimed_at, cq.claimed_by
)
SELECT cl.id, cl.backend_name, cl.object_key, cl.storage_key, cl.reason, cl.attempts, cl.size_bytes,
       cl.claimed_at, cl.claimed_by, c.reclaimed
FROM claimed cl
JOIN candidate c ON cl.id = c.id
ORDER BY cl.id;

-- name: CompleteCleanupItems :many
-- Retires the rows whose bytes are gone, each only while it is still held
-- under the claim the settler read, and returns the ids it retired. One
-- statement for a whole settle, so a request that displaced several copies
-- commits once. A row whose claim aged out and was taken over is left for its
-- new holder.
-- The three arrays are zipped by unnesting them side by side.
DELETE FROM cleanup_queue cq
WHERE (cq.id, cq.claimed_at, COALESCE(cq.claimed_by, '')) IN (
    SELECT unnest(@ids::bigint[]), unnest(@claimed_ats::timestamptz[]), unnest(@claimed_bys::text[])
)
RETURNING cq.id;

-- name: UpdateCleanupRetry :execrows
-- Schedules a retry by advancing next_retry, recording the error, and
-- clearing the claim so the row is immediately re-eligible for the next
-- worker tick. Matches the claim the settler read, so a holder that lost the
-- row cannot count an attempt against it.
UPDATE cleanup_queue
SET attempts = attempts + 1,
    next_retry = NOW() + @backoff::interval,
    last_error = @last_error,
    claimed_at = NULL,
    claimed_by = NULL
WHERE id = @id
  AND claimed_at = @claimed_at
  AND COALESCE(claimed_by, '') = @claimed_by::text;

-- name: MoveCleanupToDLQ :execrows
-- Graduates an exhausted row to the dead-letter table in one statement,
-- matching the claim the settler read so a lost holder cannot dead-letter a
-- row its new holder is still working. original_id keeps the queue row's id
-- for forensic correlation, and first_enqueued_at its created_at, so the DLQ
-- entry remembers how long the cleanup was outstanding. The bytes still count
-- as the backend's orphans while the row is dead-lettered.
WITH moved AS (
    DELETE FROM cleanup_queue cq
    WHERE cq.id = @id
      AND cq.claimed_at = @claimed_at
      AND COALESCE(cq.claimed_by, '') = @claimed_by::text
    RETURNING cq.id, cq.backend_name, cq.object_key, cq.storage_key, cq.reason, cq.size_bytes,
              cq.attempts, cq.created_at
)
INSERT INTO cleanup_dlq (
    original_id, backend_name, object_key, storage_key, reason, size_bytes,
    attempts, first_enqueued_at, last_error
)
SELECT m.id, m.backend_name, m.object_key, m.storage_key, m.reason, m.size_bytes,
       m.attempts, m.created_at, @last_error::text
FROM moved m;

-- name: CountPendingCleanups :one
SELECT COUNT(*) FROM cleanup_queue WHERE attempts < @max_attempts::int;

-- name: DeleteCleanupQueueByBackend :exec
DELETE FROM cleanup_queue WHERE backend_name = $1;

-- name: DeleteCleanupQueueByKey :execrows
-- Removes every cleanup_queue row for the (storage_key, backend_name) pair and
-- returns how many went. Rows are matched on the path rather than the object,
-- because reconcile only established that the bytes at that path are gone.
-- Sweeping by object would also drop queued deletions for the key's other
-- writes, whose bytes are still on the backend.
DELETE FROM cleanup_queue
WHERE storage_key = $1 AND backend_name = $2;

-- name: DeleteCleanupDLQByBackend :exec
DELETE FROM cleanup_dlq WHERE backend_name = $1;

-- name: CountCleanupDLQ :one
-- Returns the current depth of the cleanup_dlq table for the dashboard
-- and the cleanup_dlq_depth gauge.
SELECT COUNT(*) FROM cleanup_dlq;

-- name: ListCleanupDLQ :many
-- Read-only listing of dead-lettered cleanups for the admin cleanup-dlq
-- view. An empty backend argument returns every backend; otherwise the
-- listing is scoped to one. Newest graduations first.
SELECT backend_name, object_key, storage_key, reason, size_bytes,
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
-- and attempts to 0. An empty backend requeues every backend. Returns the
-- number of rows requeued.
WITH moved AS (
    DELETE FROM cleanup_dlq
    WHERE (sqlc.arg(backend)::text = '' OR backend_name = sqlc.arg(backend))
    RETURNING backend_name, object_key, storage_key, reason, size_bytes
)
INSERT INTO cleanup_queue (backend_name, object_key, storage_key, reason, size_bytes)
SELECT backend_name, object_key, storage_key, reason, size_bytes FROM moved;
