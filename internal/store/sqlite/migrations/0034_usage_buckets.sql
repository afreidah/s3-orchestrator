-- Keys backend_usage and backend_request_usage by the hour a charge was made in
-- (UTC) instead of a monthly period label. A backend's cycle is read as the sum
-- of its buckets from the cycle's start, so the cycle can begin on any day and
-- in any whole-hour zone without relabelling what was recorded. SQLite cannot
-- change a primary key in place, so both tables are rebuilt; each existing
-- monthly row becomes one bucket at the first instant of its month in UTC,
-- written in the store's canonical timestamp shape.

CREATE TABLE backend_usage_new (
    backend_name  TEXT NOT NULL REFERENCES backend_quotas(backend_name),
    bucket_start  TEXT NOT NULL,
    workload      TEXT NOT NULL,
    api_requests  INTEGER NOT NULL DEFAULT 0,
    egress_bytes  INTEGER NOT NULL DEFAULT 0,
    ingress_bytes INTEGER NOT NULL DEFAULT 0,
    updated_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (backend_name, bucket_start, workload)
);
INSERT INTO backend_usage_new (backend_name, bucket_start, workload, api_requests, egress_bytes, ingress_bytes, updated_at)
SELECT backend_name, period || '-01T00:00:00.000000000Z', workload, api_requests, egress_bytes, ingress_bytes, updated_at
FROM backend_usage;
DROP TABLE backend_usage;
ALTER TABLE backend_usage_new RENAME TO backend_usage;
CREATE INDEX IF NOT EXISTS idx_backend_usage_bucket_start ON backend_usage(bucket_start);

DROP INDEX IF EXISTS idx_backend_request_usage_period;
CREATE TABLE backend_request_usage_new (
    backend_name TEXT NOT NULL REFERENCES backend_quotas(backend_name),
    bucket_start TEXT NOT NULL,
    workload     TEXT NOT NULL,
    pool         TEXT NOT NULL,
    requests     INTEGER NOT NULL DEFAULT 0,
    updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (backend_name, bucket_start, workload, pool)
);
INSERT INTO backend_request_usage_new (backend_name, bucket_start, workload, pool, requests, updated_at)
SELECT backend_name, period || '-01T00:00:00.000000000Z', workload, pool, requests, updated_at
FROM backend_request_usage;
DROP TABLE backend_request_usage;
ALTER TABLE backend_request_usage_new RENAME TO backend_request_usage;
CREATE INDEX IF NOT EXISTS idx_backend_request_usage_bucket_start ON backend_request_usage(bucket_start);
