-- -------------------------------------------------------------------------------
-- Covering Indexes for the Capacity Sums
--
-- Author: Alex Freidah
--
-- backend_capacity sums size_bytes per backend over cleanup_queue and
-- cleanup_dlq for orphan bytes and over pending_objects for writes in flight,
-- on every admission check and quota read. Each table's (backend_name,
-- storage_key) index served the path lookups but not the sum, which still read
-- the table. Carrying size_bytes in the index lets the sums run from it for
-- every page the visibility map marks all-visible. The path lookups keep the
-- same leading columns, and the indexes keep their names.
-- -------------------------------------------------------------------------------

-- +goose Up
-- +goose NO TRANSACTION

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_cleanup_queue_backend_storage_key_size
    ON cleanup_queue (backend_name, storage_key) INCLUDE (size_bytes);
DROP INDEX CONCURRENTLY IF EXISTS idx_cleanup_queue_backend_storage_key;
ALTER INDEX idx_cleanup_queue_backend_storage_key_size RENAME TO idx_cleanup_queue_backend_storage_key;

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_cleanup_dlq_backend_storage_key_size
    ON cleanup_dlq (backend_name, storage_key) INCLUDE (size_bytes);
DROP INDEX CONCURRENTLY IF EXISTS idx_cleanup_dlq_backend_storage_key;
ALTER INDEX idx_cleanup_dlq_backend_storage_key_size RENAME TO idx_cleanup_dlq_backend_storage_key;

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_pending_objects_backend_storage_key_size
    ON pending_objects (backend_name, storage_key) INCLUDE (size_bytes);
DROP INDEX CONCURRENTLY IF EXISTS idx_pending_objects_backend_storage_key;
ALTER INDEX idx_pending_objects_backend_storage_key_size RENAME TO idx_pending_objects_backend_storage_key;

-- +goose Down
-- +goose NO TRANSACTION

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_cleanup_queue_backend_storage_key_narrow
    ON cleanup_queue (backend_name, storage_key);
DROP INDEX CONCURRENTLY IF EXISTS idx_cleanup_queue_backend_storage_key;
ALTER INDEX idx_cleanup_queue_backend_storage_key_narrow RENAME TO idx_cleanup_queue_backend_storage_key;

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_cleanup_dlq_backend_storage_key_narrow
    ON cleanup_dlq (backend_name, storage_key);
DROP INDEX CONCURRENTLY IF EXISTS idx_cleanup_dlq_backend_storage_key;
ALTER INDEX idx_cleanup_dlq_backend_storage_key_narrow RENAME TO idx_cleanup_dlq_backend_storage_key;

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_pending_objects_backend_storage_key_narrow
    ON pending_objects (backend_name, storage_key);
DROP INDEX CONCURRENTLY IF EXISTS idx_pending_objects_backend_storage_key;
ALTER INDEX idx_pending_objects_backend_storage_key_narrow RENAME TO idx_pending_objects_backend_storage_key;
