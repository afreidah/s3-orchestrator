-- -------------------------------------------------------------------------------
-- Managed Flag for Object Locations
--
-- Author: Alex Freidah
--
-- Reconcile imports every object it finds on a backend, including objects that
-- sit outside every configured virtual bucket prefix. Those are real bytes
-- occupying real quota, so they belong on the ledger: bytes_used is derived by
-- summing this table, and placement decisions read those totals. They are not,
-- however, the orchestrator's to act on. This flag separates the two: quota
-- sums every row, while replication, rebalance, integrity and drain only
-- consider managed ones.
--
-- Defaults to true so every pre-existing row keeps its current behaviour;
-- only reconcile sets it false, and only for keys matching no bucket prefix.
-- -------------------------------------------------------------------------------

-- +goose Up
ALTER TABLE object_locations ADD COLUMN managed BOOLEAN NOT NULL DEFAULT TRUE;

-- Backs the worker candidate scans, which all filter to managed rows.
CREATE INDEX IF NOT EXISTS idx_object_locations_managed
    ON object_locations (backend_name) WHERE managed;

-- +goose Down
DROP INDEX IF EXISTS idx_object_locations_managed;
ALTER TABLE object_locations DROP COLUMN IF EXISTS managed;
