-- -----------------------------------------------------------------------------
-- Hourly Usage Buckets
--
-- Author: Alex Freidah
--
-- Keys backend_usage and backend_request_usage by the hour a charge was made in
-- (UTC) instead of a monthly period label. A backend's cycle is read as the sum
-- of its buckets from the cycle's start, so the cycle can begin on any day and
-- in any whole-hour zone without relabelling what was recorded. Each existing
-- monthly row becomes one bucket at the first instant of its month in UTC,
-- which is where every period began until now.
-- -----------------------------------------------------------------------------

-- +goose Up
ALTER TABLE backend_usage ADD COLUMN bucket_start TIMESTAMPTZ;
UPDATE backend_usage SET bucket_start = (period || '-01T00:00:00Z')::timestamptz;
ALTER TABLE backend_usage ALTER COLUMN bucket_start SET NOT NULL,
    DROP CONSTRAINT backend_usage_pkey,
    ADD PRIMARY KEY (backend_name, bucket_start, workload),
    DROP COLUMN period;
CREATE INDEX idx_backend_usage_bucket_start ON backend_usage (bucket_start);

ALTER TABLE backend_request_usage ADD COLUMN bucket_start TIMESTAMPTZ;
UPDATE backend_request_usage SET bucket_start = (period || '-01T00:00:00Z')::timestamptz;
DROP INDEX IF EXISTS idx_backend_request_usage_period;
ALTER TABLE backend_request_usage ALTER COLUMN bucket_start SET NOT NULL,
    DROP CONSTRAINT backend_request_usage_pkey,
    ADD PRIMARY KEY (backend_name, bucket_start, workload, pool),
    DROP COLUMN period;
CREATE INDEX idx_backend_request_usage_bucket_start ON backend_request_usage (bucket_start);

-- +goose Down
DROP INDEX IF EXISTS idx_backend_usage_bucket_start;
ALTER TABLE backend_usage ADD COLUMN period TEXT;
UPDATE backend_usage SET period = to_char(bucket_start AT TIME ZONE 'UTC', 'YYYY-MM');
CREATE TABLE backend_usage_monthly AS
SELECT backend_name, period, workload, SUM(api_requests) AS api_requests,
       SUM(egress_bytes) AS egress_bytes, SUM(ingress_bytes) AS ingress_bytes, MAX(updated_at) AS updated_at
FROM backend_usage GROUP BY backend_name, period, workload;
DELETE FROM backend_usage;
ALTER TABLE backend_usage DROP CONSTRAINT backend_usage_pkey, DROP COLUMN bucket_start;
ALTER TABLE backend_usage ALTER COLUMN period SET NOT NULL,
    ADD PRIMARY KEY (backend_name, period, workload);
INSERT INTO backend_usage (backend_name, period, workload, api_requests, egress_bytes, ingress_bytes, updated_at)
SELECT backend_name, period, workload, api_requests, egress_bytes, ingress_bytes, updated_at FROM backend_usage_monthly;
DROP TABLE backend_usage_monthly;

DROP INDEX IF EXISTS idx_backend_request_usage_bucket_start;
ALTER TABLE backend_request_usage ADD COLUMN period TEXT;
UPDATE backend_request_usage SET period = to_char(bucket_start AT TIME ZONE 'UTC', 'YYYY-MM');
CREATE TABLE backend_request_usage_monthly AS
SELECT backend_name, period, workload, pool, SUM(requests) AS requests, MAX(updated_at) AS updated_at
FROM backend_request_usage GROUP BY backend_name, period, workload, pool;
DELETE FROM backend_request_usage;
ALTER TABLE backend_request_usage DROP CONSTRAINT backend_request_usage_pkey, DROP COLUMN bucket_start;
ALTER TABLE backend_request_usage ALTER COLUMN period SET NOT NULL,
    ADD PRIMARY KEY (backend_name, period, workload, pool);
INSERT INTO backend_request_usage (backend_name, period, workload, pool, requests, updated_at)
SELECT backend_name, period, workload, pool, requests, updated_at FROM backend_request_usage_monthly;
DROP TABLE backend_request_usage_monthly;
CREATE INDEX IF NOT EXISTS idx_backend_request_usage_period ON backend_request_usage(period);
