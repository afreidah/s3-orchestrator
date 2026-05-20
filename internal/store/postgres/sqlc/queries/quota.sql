-- -----------------------------------------------------------------------------
-- Quota Queries
--
-- Author: Alex Freidah
--
-- sqlc-input definitions for backend_quotas - the per-backend bytes_used,
-- bytes_limit, and orphan_bytes counters. The increment query is guarded
-- (WHERE bytes_used + delta <= bytes_limit) so an INSERT exceeding the
-- limit touches zero rows and the engine returns ErrNoSpaceAvailable. Lock
-- acquisition order across multi-row updates is enforced at the call site
-- (sorted backend_name) to avoid deadlock.
-- -----------------------------------------------------------------------------

-- name: UpsertQuotaLimit :exec
INSERT INTO backend_quotas (backend_name, bytes_limit, bytes_used, updated_at)
VALUES ($1, $2, 0, NOW())
ON CONFLICT (backend_name) DO UPDATE SET
    bytes_limit = $2,
    updated_at = NOW();

-- name: GetBackendAvailableSpace :one
-- bytes_limit = 0 means unlimited (no quota enforcement)
SELECT CASE
    WHEN q.bytes_limit = 0 THEN 9223372036854775807  -- max int64
    ELSE (q.bytes_limit - q.bytes_used - q.orphan_bytes - COALESCE(m.inflight, 0))
END::bigint AS available
FROM backend_quotas q
LEFT JOIN (
    SELECT mu.backend_name, SUM(mp.size_bytes) AS inflight
    FROM multipart_uploads mu
    JOIN multipart_parts mp ON mp.upload_id = mu.upload_id
    GROUP BY mu.backend_name
) m ON m.backend_name = q.backend_name
WHERE q.backend_name = $1;

-- name: GetLeastUtilizedBackend :one
-- Finds the backend with the lowest utilization ratio that has enough space.
SELECT q.backend_name,
       CASE WHEN q.bytes_limit = 0 THEN 9223372036854775807
            ELSE (q.bytes_limit - q.bytes_used - q.orphan_bytes - COALESCE(m.inflight, 0))
       END::bigint AS available
FROM backend_quotas q
LEFT JOIN (
    SELECT mu.backend_name, SUM(mp.size_bytes) AS inflight
    FROM multipart_uploads mu
    JOIN multipart_parts mp ON mp.upload_id = mu.upload_id
    GROUP BY mu.backend_name
) m ON m.backend_name = q.backend_name
WHERE q.backend_name = ANY(@backend_names::text[])
  AND CASE WHEN q.bytes_limit = 0 THEN 9223372036854775807
           ELSE (q.bytes_limit - q.bytes_used - q.orphan_bytes - COALESCE(m.inflight, 0))
      END >= @min_size::bigint
ORDER BY CASE WHEN q.bytes_limit = 0 THEN 0
              ELSE (q.bytes_used + q.orphan_bytes)::float8 / q.bytes_limit::float8
         END ASC
LIMIT 1;

-- name: GetAllQuotaStats :many
SELECT backend_name, bytes_used, bytes_limit, orphan_bytes, updated_at
FROM backend_quotas;

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

-- name: IncrementQuota :execrows
UPDATE backend_quotas
SET bytes_used = bytes_used + @amount, updated_at = NOW()
WHERE backend_name = @backend_name
  AND (bytes_limit = 0 OR bytes_used + orphan_bytes + @amount <= bytes_limit);

-- name: DecrementQuota :exec
UPDATE backend_quotas
SET bytes_used = GREATEST(0, bytes_used - @amount), updated_at = NOW()
WHERE backend_name = @backend_name;

-- name: AdjustBackendBytesUsed :exec
-- Applies a signed delta to bytes_used without the bytes_limit guard
-- IncrementQuota uses, because the on-disk byte count is reality and
-- the counter must follow it whether or not the limit would otherwise
-- be exceeded. GREATEST(0, ...) clamps so a delta-arithmetic bug
-- cannot drive the counter negative. Used by MarkObjectEncrypted and
-- MarkObjectDecrypted to keep bytes_used in step with size_bytes
-- after the bulk encrypt-existing / decrypt-existing rewrite path
-- changes the on-disk size.
UPDATE backend_quotas
SET bytes_used = GREATEST(0, bytes_used + @delta), updated_at = NOW()
WHERE backend_name = @backend_name;

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

-- name: DeleteQuota :exec
DELETE FROM backend_quotas WHERE backend_name = $1;
