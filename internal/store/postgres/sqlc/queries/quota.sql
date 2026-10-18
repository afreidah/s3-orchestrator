-- -----------------------------------------------------------------------------
-- Quota Queries
--
-- Author: Alex Freidah
--
-- sqlc-input definitions for backend_quotas - the per-backend bytes_limit - and
-- backend_quota_stripes, which holds the stored byte total split across rows so
-- concurrent writers do not contend on one. Orphan bytes are read from
-- backend_capacity, which sums the cleanup rows still to delete.
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
SELECT c.backend_name,
       c.bytes_used,
       c.bytes_limit,
       c.orphan_bytes,
       q.updated_at
FROM backend_capacity c
JOIN backend_quotas q ON q.backend_name = c.backend_name;

-- name: LedgerGroups :many
-- The ledger in one grouped pass, by backend, stored form and managed flag, so
-- the dashboard and the fleet snapshot read the table once. What each figure
-- counts is decided in Go from the groups, so both engines report the same
-- figures from the same rule. The oldest touch falls back to created_at as the
-- scrub queue ordering does, so a never-scrubbed copy is measured from when it
-- was written.
SELECT backend_name, form, managed,
    COUNT(*)::bigint AS copies,
    COALESCE(SUM(size_bytes), 0)::bigint AS stored_bytes,
    COALESCE(SUM(object_size), 0)::bigint AS object_bytes,
    COUNT(*) FILTER (WHERE last_scrubbed_at IS NULL)::bigint AS never_scrubbed,
    MIN(COALESCE(last_scrubbed_at, created_at))::timestamptz AS oldest_touched
FROM object_locations
GROUP BY backend_name, form, managed;

-- name: GetActiveMultipartCountsByBackend :many
SELECT backend_name, COUNT(*) AS upload_count
FROM multipart_uploads
GROUP BY backend_name;

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
-- awaiting cleanup, and the writes that have not landed yet. The figures come
-- from backend_capacity, the same view admission tests against, and they are
-- rows every instance can read, which is what makes them fleet-wide.
SELECT backend_name, bytes_limit, bytes_used, orphan_bytes, inflight_bytes
FROM backend_capacity;

-- name: DeleteQuota :exec
DELETE FROM backend_quotas WHERE backend_name = $1;
