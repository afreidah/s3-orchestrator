-- +goose Up
-- -----------------------------------------------------------------------------
-- Per-Pool Request Usage
--
-- Author: Alex Freidah
--
-- backend_usage.api_requests counts every call made against a backend, which
-- is the right figure for reporting but the wrong one for admission: providers
-- meter operations in classes with separate allowances, and charging a delete
-- against an upload budget is what locks a backend out while its read
-- allowance sits unused.
--
-- Request budgets are named in config, so the count they accumulate is keyed
-- rather than columnar. Pools are additive - an operation charges every pool
-- containing it - so these rows do not sum to backend_usage.api_requests and
-- are not a decomposition of it.
--
-- Bytes stay in backend_usage: providers do not class bytes.
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS backend_request_usage (
    backend_name TEXT NOT NULL REFERENCES backend_quotas(backend_name),
    period       TEXT NOT NULL,
    pool         TEXT NOT NULL,
    requests     BIGINT NOT NULL DEFAULT 0,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (backend_name, period, pool)
);

-- The flush writes per (backend, period, pool) and the admission baseline
-- loads a whole period at once; the primary key serves the first, this serves
-- the second.
CREATE INDEX IF NOT EXISTS idx_backend_request_usage_period
    ON backend_request_usage(period);

-- +goose Down
DROP INDEX IF EXISTS idx_backend_request_usage_period;
DROP TABLE IF EXISTS backend_request_usage;
