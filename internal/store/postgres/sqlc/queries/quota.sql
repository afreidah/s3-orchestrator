-- -----------------------------------------------------------------------------
-- Quota Queries
--
-- Author: Alex Freidah
--
-- sqlc-input definitions for backend_quotas - the per-backend bytes_limit and
-- orphan_bytes counters - and backend_quota_stripes, which holds the stored
-- byte total split across rows so concurrent writers do not contend on one.
--
-- A backend's byte total is always SUM(bytes_used) over its stripes, never a
-- single row, and the clamp at zero belongs on that sum: a stripe is signed and
-- may sit negative while the total is correct. Lock acquisition order across
-- multi-row updates is enforced at the call site (sorted backend_name) to avoid
-- deadlock.
-- -----------------------------------------------------------------------------

-- name: UpsertQuotaLimit :exec
INSERT INTO backend_quotas (backend_name, bytes_limit, updated_at)
VALUES ($1, $2, NOW())
ON CONFLICT (backend_name) DO UPDATE SET
    bytes_limit = $2,
    updated_at = NOW();

-- name: AdjustQuotaStripe :exec
-- Applies a signed delta to one stripe, materializing the row on first use so
-- nothing has to seed a backend's stripes up front. No bytes_limit guard: the
-- ceiling is enforced before the write is admitted, and a counter that declined
-- to record bytes already on a backend would understate it permanently.
INSERT INTO backend_quota_stripes (backend_name, stripe_id, bytes_used)
VALUES (@backend_name, @stripe_id, @delta)
ON CONFLICT (backend_name, stripe_id) DO UPDATE
SET bytes_used = backend_quota_stripes.bytes_used + EXCLUDED.bytes_used;

-- name: GetAllQuotaStats :many
SELECT q.backend_name,
       GREATEST(0, COALESCE(s.bytes_used, 0))::bigint AS bytes_used,
       q.bytes_limit,
       q.orphan_bytes,
       q.updated_at
FROM backend_quotas q
LEFT JOIN (
    SELECT backend_name, SUM(bytes_used) AS bytes_used
    FROM backend_quota_stripes
    GROUP BY backend_name
) s ON s.backend_name = q.backend_name;

-- name: GetObjectCountsByBackend :many
SELECT backend_name, COUNT(*) AS object_count
FROM object_locations
GROUP BY backend_name;

-- name: GetActiveMultipartCountsByBackend :many
SELECT backend_name, COUNT(*) AS upload_count
FROM multipart_uploads
GROUP BY backend_name;

-- name: GetUnverifiedObjectCountsByBackend :many
SELECT backend_name, COUNT(*) AS object_count
FROM object_locations
WHERE content_hash IS NULL
GROUP BY backend_name;

-- name: GetObjectSizeBytes :one
-- Returns the current size_bytes of an object_locations row. Used
-- inside MarkObjectDecrypted so the caller can compute the size
-- delta against the row that is about to be overwritten without
-- needing the old ciphertext size to flow through the API.
SELECT size_bytes FROM object_locations
WHERE object_key = $1 AND backend_name = $2;

-- name: IncrementOrphanBytes :exec
UPDATE backend_quotas
SET orphan_bytes = orphan_bytes + @amount, updated_at = NOW()
WHERE backend_name = @backend_name;

-- name: DecrementOrphanBytes :exec
UPDATE backend_quotas
SET orphan_bytes = GREATEST(0, orphan_bytes - @amount), updated_at = NOW()
WHERE backend_name = @backend_name;

-- name: SumObjectSizesByBackend :many
-- Authoritative per-backend byte total from the object ledger. Used by usage
-- reconciliation to recompute bytes_used, which is otherwise an incrementally
-- maintained counter that drifts if any mutation path misses an adjustment.
SELECT backend_name, COALESCE(SUM(size_bytes), 0)::bigint AS total_bytes
FROM object_locations
GROUP BY backend_name;

-- name: SetBackendBytesUsed :exec
-- Replaces a backend's byte total with an authoritative recomputed value by
-- collapsing it onto stripe zero and clearing the rest. Reconciliation is the
-- one caller: it has recomputed the total from the ledger, so the distribution
-- that produced the old value carries no information worth preserving.
WITH cleared AS (
    UPDATE backend_quota_stripes
    SET bytes_used = 0
    WHERE backend_name = @backend_name AND stripe_id <> 0
    RETURNING 1
)
INSERT INTO backend_quota_stripes (backend_name, stripe_id, bytes_used)
VALUES (@backend_name, 0, @bytes_used)
ON CONFLICT (backend_name, stripe_id) DO UPDATE
SET bytes_used = EXCLUDED.bytes_used;

-- name: ListBackendQuotaUsage :many
-- Every backend's ceiling and what occupies it: the striped byte total, orphans
-- awaiting cleanup, and the writes that have not landed yet.
--
-- In-flight is the parts of incomplete multipart uploads plus the intents of
-- single-object PUTs still in progress. Both describe bytes on their way to a
-- backend that no object_locations row covers, and both are rows every instance
-- can read - which is what makes the figure fleet-wide rather than a view of
-- what this process happens to have started.
SELECT q.backend_name,
       q.bytes_limit,
       GREATEST(0, COALESCE(s.bytes_used, 0))::bigint AS bytes_used,
       q.orphan_bytes,
       (COALESCE(m.inflight, 0) + COALESCE(p.inflight, 0))::bigint AS inflight_bytes
FROM backend_quotas q
LEFT JOIN (
    SELECT backend_name, SUM(bytes_used) AS bytes_used
    FROM backend_quota_stripes
    GROUP BY backend_name
) s ON s.backend_name = q.backend_name
LEFT JOIN (
    SELECT mu.backend_name, SUM(mp.size_bytes) AS inflight
    FROM multipart_uploads mu
    JOIN multipart_parts mp ON mp.upload_id = mu.upload_id
    GROUP BY mu.backend_name
) m ON m.backend_name = q.backend_name
LEFT JOIN (
    SELECT backend_name, SUM(size_bytes) AS inflight
    FROM pending_objects
    GROUP BY backend_name
) p ON p.backend_name = q.backend_name;

-- name: DeleteQuota :exec
DELETE FROM backend_quotas WHERE backend_name = $1;
