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
-- Returns usage stats for all backends in the given period (e.g. '2026-02').
SELECT backend_name, api_requests, egress_bytes, ingress_bytes
FROM backend_usage
WHERE period = @period;

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

-- name: GetPoolUsageForPeriod :many
-- Returns every pool's request count for every backend in the given period.
SELECT backend_name, pool, requests
FROM backend_request_usage
WHERE period = @period;

-- name: DeletePoolUsageByBackend :exec
DELETE FROM backend_request_usage WHERE backend_name = $1;
