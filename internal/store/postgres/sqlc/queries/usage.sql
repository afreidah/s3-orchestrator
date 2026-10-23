-- -----------------------------------------------------------------------------
-- Usage Queries
--
-- Author: Alex Freidah
--
-- sqlc-input definitions for backend_usage and backend_request_usage - the
-- per-backend (api_requests, egress_bytes, ingress_bytes) counters and per-pool
-- request counts, kept per hourly bucket in UTC, used by the usage flusher, the
-- limit checks and the dashboard. FlushUsageDeltas uses INSERT ON CONFLICT DO
-- UPDATE with column-level ADDs so multiple flushers can converge on the same
-- row without losing deltas.
-- -----------------------------------------------------------------------------

-- name: FlushUsageDeltas :exec
-- Atomically adds accumulated in-memory deltas to the persistent usage row.
-- Creates the row if it doesn't exist for this (backend, bucket, workload) yet.
INSERT INTO backend_usage (backend_name, bucket_start, workload, api_requests, egress_bytes, ingress_bytes, updated_at)
VALUES (@backend_name, @bucket_start, @workload, @api_requests, @egress_bytes, @ingress_bytes, NOW())
ON CONFLICT (backend_name, bucket_start, workload) DO UPDATE SET
    api_requests  = backend_usage.api_requests  + @api_requests,
    egress_bytes  = backend_usage.egress_bytes  + @egress_bytes,
    ingress_bytes = backend_usage.ingress_bytes + @ingress_bytes,
    updated_at    = NOW();

-- name: GetUsageSince :many
-- Sums each backend's buckets from its own start onward, by workload, joined to
-- its pool sums, so a workload with pools repeats its totals. The backends and
-- their starts are zipped by unnesting the two arrays side by side; a backend
-- not listed is not read.
WITH win AS (
    SELECT unnest(@backends::text[]) AS backend_name, unnest(@starts::timestamptz[]) AS start
),
totals AS (
    SELECT u.backend_name, u.workload,
           SUM(u.api_requests)::bigint AS api_requests,
           SUM(u.egress_bytes)::bigint AS egress_bytes,
           SUM(u.ingress_bytes)::bigint AS ingress_bytes
    FROM backend_usage u
    JOIN win ON win.backend_name = u.backend_name AND u.bucket_start >= win.start
    GROUP BY u.backend_name, u.workload
),
pools AS (
    SELECT p.backend_name, p.workload, p.pool, SUM(p.requests)::bigint AS requests
    FROM backend_request_usage p
    JOIN win ON win.backend_name = p.backend_name AND p.bucket_start >= win.start
    GROUP BY p.backend_name, p.workload, p.pool
)
SELECT t.backend_name, t.workload, t.api_requests, t.egress_bytes, t.ingress_bytes,
       p.pool, p.requests
FROM totals t
LEFT JOIN pools p ON p.backend_name = t.backend_name AND p.workload = t.workload;

-- name: DeleteUsageByBackend :exec
DELETE FROM backend_usage WHERE backend_name = $1;

-- name: FlushPoolDelta :exec
-- Adds one pool's accumulated delta to its persistent row, creating the row on
-- first charge in the bucket. Same additive ON CONFLICT as the totals above so
-- concurrent flushers converge.
INSERT INTO backend_request_usage (backend_name, bucket_start, workload, pool, requests, updated_at)
VALUES (@backend_name, @bucket_start, @workload, @pool, @requests, NOW())
ON CONFLICT (backend_name, bucket_start, workload, pool) DO UPDATE SET
    requests   = backend_request_usage.requests + @requests,
    updated_at = NOW();

-- name: DeletePoolUsageByBackend :exec
DELETE FROM backend_request_usage WHERE backend_name = $1;

-- name: PruneUsageBuckets :exec
-- Drops one backend's buckets that started before the cutoff. Keyed by the
-- primary key's leading columns, so the flush that runs it reads only what it
-- deletes.
DELETE FROM backend_usage WHERE backend_name = @backend_name AND bucket_start < @cutoff;

-- name: PrunePoolUsageBuckets :exec
-- Drops one backend's pool buckets that started before the cutoff.
DELETE FROM backend_request_usage WHERE backend_name = @backend_name AND bucket_start < @cutoff;

-- name: RecordUsageFlush :execrows
-- Records a shared batch's id in the transaction that applies it. Zero rows
-- means the batch was already applied, and its deltas must not be added again.
INSERT INTO usage_flushes (flush_id) VALUES (@flush_id)
ON CONFLICT (flush_id) DO NOTHING;

-- name: PruneUsageFlushes :exec
-- Drops ids older than any batch that can still be staged, whose keys expire
-- after 35 days.
DELETE FROM usage_flushes WHERE applied_at < NOW() - INTERVAL '40 days';
