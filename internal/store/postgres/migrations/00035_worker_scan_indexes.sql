-- -------------------------------------------------------------------------------
-- Indexes for the Drain, Reconcile and Cleanup Scans
--
-- Author: Alex Freidah
--
-- Drain and purge page through a backend's managed rows smallest first, by
-- (size_bytes, object_key). The managed index they used filtered without
-- ordering, so every page read and sorted all of the backend's rows. The new
-- one serves the order and the cursor, and its leading column still serves the
-- drain-completion check.
--
-- Reconcile and the cleanup sweep look up queued and dead-lettered deletes by
-- path, which neither table indexed. The DLQ's backend index is subsumed by the
-- new one's leading column.
--
-- idx_object_locations_backend duplicates the leading column of the unique
-- (backend_name, storage_key) index.
-- -------------------------------------------------------------------------------

-- +goose Up
-- +goose NO TRANSACTION

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_object_locations_managed_size
    ON object_locations (backend_name, size_bytes, object_key) WHERE managed;

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_cleanup_queue_backend_storage_key
    ON cleanup_queue (backend_name, storage_key);

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_cleanup_dlq_backend_storage_key
    ON cleanup_dlq (backend_name, storage_key);

DROP INDEX CONCURRENTLY IF EXISTS idx_object_locations_managed;
DROP INDEX CONCURRENTLY IF EXISTS idx_object_locations_backend;
DROP INDEX CONCURRENTLY IF EXISTS idx_cleanup_dlq_backend;

-- +goose Down
-- +goose NO TRANSACTION

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_cleanup_dlq_backend
    ON cleanup_dlq (backend_name);

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_object_locations_backend
    ON object_locations (backend_name);

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_object_locations_managed
    ON object_locations (backend_name) WHERE managed;

DROP INDEX CONCURRENTLY IF EXISTS idx_cleanup_dlq_backend_storage_key;
DROP INDEX CONCURRENTLY IF EXISTS idx_cleanup_queue_backend_storage_key;
DROP INDEX CONCURRENTLY IF EXISTS idx_object_locations_managed_size;
