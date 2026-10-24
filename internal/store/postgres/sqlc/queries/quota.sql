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
-- Every backend's ceiling and what occupies it: the striped byte total, orphans
-- awaiting cleanup, and the writes that have not landed yet. The figures come
-- from backend_capacity, the same view admission tests against, and they are
-- rows every instance can read, which is what makes them fleet-wide.
SELECT backend_name, bytes_limit, bytes_used, orphan_bytes, inflight_bytes
FROM backend_capacity;

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

-- name: ReconcileUsage :many
-- Brings each backend's striped total to the ledger's sum of its copies and
-- returns the difference applied, for the backends that had drifted. Both sums
-- are read from the one snapshot this statement takes, and the difference is
-- added to stripe zero rather than written over the total, so a write that
-- commits alongside keeps its own adjustment on whichever stripe it charged. A
-- backend with no copies left is brought to zero. The insert is a writable CTE,
-- which Postgres runs whether or not the outer select reads it.
WITH ledger AS (
    SELECT backend_name, SUM(size_bytes) AS total
    FROM object_locations
    GROUP BY backend_name
),
striped AS (
    SELECT q.backend_name, COALESCE(SUM(s.bytes_used), 0) AS total
    FROM backend_quotas q
    LEFT JOIN backend_quota_stripes s ON s.backend_name = q.backend_name
    GROUP BY q.backend_name
),
drift AS (
    SELECT st.backend_name, (COALESCE(l.total, 0) - st.total)::bigint AS delta
    FROM striped st
    LEFT JOIN ledger l ON l.backend_name = st.backend_name
    WHERE COALESCE(l.total, 0) <> st.total
),
applied AS (
    INSERT INTO backend_quota_stripes (backend_name, stripe_id, bytes_used)
    SELECT backend_name, 0, delta FROM drift
    ON CONFLICT (backend_name, stripe_id) DO UPDATE
    SET bytes_used = backend_quota_stripes.bytes_used + EXCLUDED.bytes_used
)
SELECT backend_name, delta FROM drift;

-- name: LockBackendForClaim :exec
-- Serializes the claims on one backend for the rest of the transaction, so
-- the room check that follows sees every claim committed before it. Only a
-- backend with a byte ceiling is locked: a claim on an unlimited one cannot
-- overrun anything, and leaving it unlocked keeps its claims concurrent.
SELECT pg_advisory_xact_lock(@lock_space::int, hashtext(backend_name))
FROM backend_quotas
WHERE backend_name = @backend_name AND bytes_limit > 0;

-- name: DeleteQuota :exec
DELETE FROM backend_quotas WHERE backend_name = $1;
