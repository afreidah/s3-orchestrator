-- Keys backend_usage and backend_request_usage by the workload that made the
-- charge as well as backend and period. SQLite cannot change a primary key in
-- place, so both tables are rebuilt; existing rows become the unknown
-- workload.

CREATE TABLE backend_usage_new (
    backend_name  TEXT NOT NULL REFERENCES backend_quotas(backend_name),
    period        TEXT NOT NULL,
    workload      TEXT NOT NULL,
    api_requests  INTEGER NOT NULL DEFAULT 0,
    egress_bytes  INTEGER NOT NULL DEFAULT 0,
    ingress_bytes INTEGER NOT NULL DEFAULT 0,
    updated_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (backend_name, period, workload)
);
INSERT INTO backend_usage_new (backend_name, period, workload, api_requests, egress_bytes, ingress_bytes, updated_at)
SELECT backend_name, period, 'unknown', api_requests, egress_bytes, ingress_bytes, updated_at FROM backend_usage;
DROP TABLE backend_usage;
ALTER TABLE backend_usage_new RENAME TO backend_usage;

CREATE TABLE backend_request_usage_new (
    backend_name TEXT NOT NULL REFERENCES backend_quotas(backend_name),
    period       TEXT NOT NULL,
    workload     TEXT NOT NULL,
    pool         TEXT NOT NULL,
    requests     INTEGER NOT NULL DEFAULT 0,
    updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (backend_name, period, workload, pool)
);
INSERT INTO backend_request_usage_new (backend_name, period, workload, pool, requests, updated_at)
SELECT backend_name, period, 'unknown', pool, requests, updated_at FROM backend_request_usage;
DROP TABLE backend_request_usage;
ALTER TABLE backend_request_usage_new RENAME TO backend_request_usage;

CREATE INDEX IF NOT EXISTS idx_backend_request_usage_period
    ON backend_request_usage(period);
