-- -----------------------------------------------------------------------------
-- Usage by Workload
--
-- Author: Alex Freidah
--
-- Keys backend_usage and backend_request_usage by the workload that made the
-- charge as well as backend and period. A backend's totals are the sum of its
-- workloads. Existing rows become the unknown workload.
-- -----------------------------------------------------------------------------

-- +goose Up
ALTER TABLE backend_usage ADD COLUMN workload TEXT NOT NULL DEFAULT 'unknown';
ALTER TABLE backend_usage ALTER COLUMN workload DROP DEFAULT;
ALTER TABLE backend_usage DROP CONSTRAINT backend_usage_pkey,
    ADD PRIMARY KEY (backend_name, period, workload);

ALTER TABLE backend_request_usage ADD COLUMN workload TEXT NOT NULL DEFAULT 'unknown';
ALTER TABLE backend_request_usage ALTER COLUMN workload DROP DEFAULT;
ALTER TABLE backend_request_usage DROP CONSTRAINT backend_request_usage_pkey,
    ADD PRIMARY KEY (backend_name, period, workload, pool);

-- +goose Down
INSERT INTO backend_usage (backend_name, period, workload, api_requests, egress_bytes, ingress_bytes)
SELECT backend_name, period, '', SUM(api_requests), SUM(egress_bytes), SUM(ingress_bytes)
FROM backend_usage GROUP BY backend_name, period;
DELETE FROM backend_usage WHERE workload <> '';
ALTER TABLE backend_usage DROP CONSTRAINT backend_usage_pkey,
    ADD PRIMARY KEY (backend_name, period);
ALTER TABLE backend_usage DROP COLUMN workload;

INSERT INTO backend_request_usage (backend_name, period, workload, pool, requests)
SELECT backend_name, period, '', pool, SUM(requests)
FROM backend_request_usage GROUP BY backend_name, period, pool;
DELETE FROM backend_request_usage WHERE workload <> '';
ALTER TABLE backend_request_usage DROP CONSTRAINT backend_request_usage_pkey,
    ADD PRIMARY KEY (backend_name, period, pool);
ALTER TABLE backend_request_usage DROP COLUMN workload;
