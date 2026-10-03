-- -------------------------------------------------------------------------------
-- Indexes for the Per-Write Storage Key
--
-- Author: Alex Freidah
--
-- Reconcile walks the ledger by storage_key, since a backend listing returns
-- paths rather than object keys. The walk sorts with COLLATE "C" because S3
-- lists keys in byte order; a locale-collated walk would mis-pair keys.
--
-- The unique index enforces one object per path per backend. Existing rows
-- satisfy it already: each was backfilled with its object key, and
-- (object_key, backend_name) is the primary key.
-- -------------------------------------------------------------------------------

-- +goose Up
-- +goose NO TRANSACTION

-- Backs ListObjectsByBackendKeyAsc: WHERE backend_name = $1
-- AND storage_key COLLATE "C" > $2 ORDER BY storage_key COLLATE "C" ASC.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_object_locations_backend_storage_key_collate_c
    ON object_locations (backend_name, storage_key COLLATE "C");

CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_object_locations_backend_storage_key_unique
    ON object_locations (backend_name, storage_key);

DROP INDEX CONCURRENTLY IF EXISTS idx_object_locations_backend_key_collate_c;

-- +goose Down
-- +goose NO TRANSACTION

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_object_locations_backend_key_collate_c
    ON object_locations (backend_name, object_key COLLATE "C");

DROP INDEX CONCURRENTLY IF EXISTS idx_object_locations_backend_storage_key_unique;
DROP INDEX CONCURRENTLY IF EXISTS idx_object_locations_backend_storage_key_collate_c;
