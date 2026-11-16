-- -----------------------------------------------------------------------------
-- Quota Queries
--
-- Author: Alex Freidah
--
-- sqlc-input definitions for backend_quotas - the per-backend bytes_limit - and
-- backend_quota_stripes, which holds the ledger's figures split across rows so
-- concurrent writers do not contend on one, and by the stored form and managed
-- flag of the copies each row counts. Orphan bytes are read from
-- backend_capacity, which sums the cleanup rows still to delete.
--
-- A figure is always a sum over rows, never a single row, and the clamp at zero
-- belongs on that sum: a row is signed and may sit negative while the total is
-- correct. Lock acquisition order across multi-row updates is enforced at the
-- call site (sorted by backend, stripe, form and managed) to avoid deadlock.
-- -----------------------------------------------------------------------------

-- name: UpsertQuotaLimit :exec
INSERT INTO backend_quotas (backend_name, bytes_limit, updated_at)
VALUES ($1, $2, NOW())
ON CONFLICT (backend_name) DO UPDATE SET
    bytes_limit = $2,
    updated_at = NOW();

-- name: AdjustQuotaStripes :exec
-- Applies signed deltas to counter rows, materializing each row on first use so
-- nothing has to seed a backend's rows up front. No bytes_limit guard: the
-- ceiling is enforced before the write is admitted, and a counter that declined
-- to record bytes already on a backend would understate it permanently.
-- The arrays are zipped by unnesting them side by side, and the rows are written
-- in array order, which is the order concurrent charges rely on not to deadlock.
INSERT INTO backend_quota_stripes (backend_name, stripe_id, form, managed, copies, bytes_used, object_bytes, never_scrubbed)
SELECT unnest(@backend_names::text[]), unnest(@stripe_ids::smallint[]), unnest(@forms::bigint[]),
    unnest(@managed::bool[]), unnest(@copies::bigint[]), unnest(@stored_bytes::bigint[]),
    unnest(@object_bytes::bigint[]), unnest(@never_scrubbed::bigint[])
ON CONFLICT (backend_name, stripe_id, form, managed) DO UPDATE
SET copies = backend_quota_stripes.copies + EXCLUDED.copies,
    bytes_used = backend_quota_stripes.bytes_used + EXCLUDED.bytes_used,
    object_bytes = backend_quota_stripes.object_bytes + EXCLUDED.object_bytes,
    never_scrubbed = backend_quota_stripes.never_scrubbed + EXCLUDED.never_scrubbed;

-- name: GetAllQuotaStats :many
-- Every backend's ceiling and what occupies it: the striped byte total, orphans
-- awaiting cleanup, and the writes that have not landed yet. The figures come
-- from backend_capacity, the same view admission tests against, and they are
-- rows every instance can read, which is what makes them fleet-wide.
SELECT backend_name, bytes_limit, bytes_used, orphan_bytes, inflight_bytes
FROM backend_capacity;

-- name: LedgerGroups :many
-- The ledger's counters summed by backend, stored form and managed flag. What
-- each figure counts is decided in Go from the groups, so both engines report
-- the same figures from the same rule. A group whose copies sum to zero holds
-- nothing and is left out.
SELECT backend_name, form, managed,
    SUM(copies)::bigint AS copies,
    SUM(bytes_used)::bigint AS stored_bytes,
    SUM(object_bytes)::bigint AS object_bytes,
    SUM(never_scrubbed)::bigint AS never_scrubbed
FROM backend_quota_stripes
GROUP BY backend_name, form, managed
HAVING SUM(copies) <> 0;

-- name: GetActiveMultipartCountsByBackend :many
SELECT backend_name, COUNT(*) AS upload_count
FROM multipart_uploads
GROUP BY backend_name;

-- name: ReconcileUsage :many
-- Brings every counter to what the ledger's copies hold, by backend, stored
-- form and managed flag, and returns each drifted backend with the stored bytes
-- applied to it. Both sides are read from the one snapshot this statement
-- takes, and the difference is added to stripe zero rather than written over
-- the counters, so a write that commits alongside keeps its own adjustment on
-- whichever row it charged. A group with no copies left is brought to zero, and
-- only backends with a quota row are counted. The insert is a writable CTE,
-- which Postgres runs whether or not the outer select reads it. It takes the
-- rows in the order every counter write uses, so it cannot deadlock against a
-- batch delete.
WITH ledger AS (
    SELECT backend_name, form, managed, COUNT(*) AS copies, SUM(size_bytes) AS stored_bytes,
        SUM(object_size) AS object_bytes, COUNT(*) FILTER (WHERE last_scrubbed_at IS NULL) AS never_scrubbed
    FROM object_locations
    GROUP BY backend_name, form, managed
),
counted AS (
    SELECT backend_name, form, managed, SUM(copies) AS copies, SUM(bytes_used) AS stored_bytes,
        SUM(object_bytes) AS object_bytes, SUM(never_scrubbed) AS never_scrubbed
    FROM backend_quota_stripes
    GROUP BY backend_name, form, managed
),
drift AS (
    SELECT COALESCE(l.backend_name, c.backend_name) AS backend_name,
        COALESCE(l.form, c.form) AS form, COALESCE(l.managed, c.managed) AS managed,
        (COALESCE(l.copies, 0) - COALESCE(c.copies, 0))::bigint AS copies,
        (COALESCE(l.stored_bytes, 0) - COALESCE(c.stored_bytes, 0))::bigint AS stored_bytes,
        (COALESCE(l.object_bytes, 0) - COALESCE(c.object_bytes, 0))::bigint AS object_bytes,
        (COALESCE(l.never_scrubbed, 0) - COALESCE(c.never_scrubbed, 0))::bigint AS never_scrubbed
    FROM ledger l
    FULL JOIN counted c ON c.backend_name = l.backend_name AND c.form = l.form AND c.managed = l.managed
),
drifted AS (
    SELECT * FROM drift
    WHERE backend_name IN (SELECT backend_name FROM backend_quotas)
      AND (copies <> 0 OR stored_bytes <> 0 OR object_bytes <> 0 OR never_scrubbed <> 0)
),
applied AS (
    INSERT INTO backend_quota_stripes (backend_name, stripe_id, form, managed, copies, bytes_used, object_bytes, never_scrubbed)
    SELECT backend_name, 0, form, managed, copies, stored_bytes, object_bytes, never_scrubbed FROM drifted
    ORDER BY backend_name COLLATE "C", form, managed
    ON CONFLICT (backend_name, stripe_id, form, managed) DO UPDATE
    SET copies = backend_quota_stripes.copies + EXCLUDED.copies,
        bytes_used = backend_quota_stripes.bytes_used + EXCLUDED.bytes_used,
        object_bytes = backend_quota_stripes.object_bytes + EXCLUDED.object_bytes,
        never_scrubbed = backend_quota_stripes.never_scrubbed + EXCLUDED.never_scrubbed
)
SELECT backend_name, SUM(stored_bytes)::bigint AS delta FROM drifted GROUP BY backend_name;

-- name: LockBackendsForClaim :exec
-- Serializes the claims on these backends for the rest of the transaction, so
-- the room check that follows sees every claim committed before it. Only a
-- backend with a byte ceiling is locked: a claim on an unlimited one cannot
-- overrun anything, and leaving it unlocked keeps its claims concurrent. The
-- byte order keeps claims on overlapping sets from deadlocking.
SELECT pg_advisory_xact_lock(@lock_space::int, hashtext(locking.backend_name))
FROM (
    SELECT backend_name FROM backend_quotas
    WHERE backend_name = ANY(@backend_names::text[]) AND bytes_limit > 0
    ORDER BY backend_name COLLATE "C"
) locking;

-- name: DeleteQuota :exec
DELETE FROM backend_quotas WHERE backend_name = $1;
