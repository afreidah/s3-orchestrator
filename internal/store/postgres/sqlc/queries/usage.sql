-- -----------------------------------------------------------------------------
-- Usage Queries
--
-- Author: Alex Freidah
--
-- sqlc-input definitions for backend_usage - the monthly per-backend
-- (api_requests, egress_bytes, ingress_bytes) counters used by the usage
-- flusher and the dashboard. FlushUsageDeltas uses INSERT ON CONFLICT DO
-- UPDATE with column-level ADDs so multiple flushers can converge on the
-- same row without losing deltas.
-- -----------------------------------------------------------------------------

-- name: FlushUsageDeltas :exec
-- Atomically adds accumulated in-memory deltas to the persistent usage row.
-- Creates the row if it doesn't exist for this (backend, period) yet.
INSERT INTO backend_usage (backend_name, period, api_requests, egress_bytes, ingress_bytes, updated_at)
VALUES (@backend_name, @period, @api_requests, @egress_bytes, @ingress_bytes, NOW())
ON CONFLICT (backend_name, period) DO UPDATE SET
    api_requests  = backend_usage.api_requests  + @api_requests,
    egress_bytes  = backend_usage.egress_bytes  + @egress_bytes,
    ingress_bytes = backend_usage.ingress_bytes + @ingress_bytes,
    updated_at    = NOW();

-- name: GetUsageForPeriod :many
-- Returns every backend's totals for the given period (e.g. '2026-02'), joined
-- to its pool rows, so a backend with pools repeats its totals. Every pool row
-- has a totals row: a flush writes both in one transaction.
SELECT u.backend_name, u.api_requests, u.egress_bytes, u.ingress_bytes,
       p.pool, p.requests
FROM backend_usage u
LEFT JOIN backend_request_usage p ON p.backend_name = u.backend_name AND p.period = u.period
WHERE u.period = @period;

-- name: DeleteUsageByBackend :exec
DELETE FROM backend_usage WHERE backend_name = $1;

-- name: FlushPoolDelta :exec
-- Adds one pool's accumulated delta to its persistent row, creating the row on
-- first charge in the period. Same additive ON CONFLICT as the totals above so
-- concurrent flushers converge.
INSERT INTO backend_request_usage (backend_name, period, pool, requests, updated_at)
VALUES (@backend_name, @period, @pool, @requests, NOW())
ON CONFLICT (backend_name, period, pool) DO UPDATE SET
    requests   = backend_request_usage.requests + @requests,
    updated_at = NOW();

-- name: DeletePoolUsageByBackend :exec
DELETE FROM backend_request_usage WHERE backend_name = $1;

-- name: RecordUsageFlush :execrows
-- Records a shared batch's id in the transaction that applies it. Zero rows
-- means the batch was already applied, and its deltas must not be added again.
INSERT INTO usage_flushes (flush_id) VALUES (@flush_id)
ON CONFLICT (flush_id) DO NOTHING;

-- name: PruneUsageFlushes :exec
-- Drops ids older than any batch that can still be staged, whose keys expire
-- after 35 days.
DELETE FROM usage_flushes WHERE applied_at < NOW() - INTERVAL '40 days';
