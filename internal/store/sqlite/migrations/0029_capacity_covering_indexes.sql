-- backend_capacity sums size_bytes per backend over cleanup_queue and
-- cleanup_dlq for orphan bytes and over pending_objects for writes in flight,
-- on every admission check. Carrying size_bytes as the last column of each
-- table's (backend_name, storage_key) index lets the sums run from the index
-- alone; the path lookups keep the same leading columns and the indexes keep
-- their names.

DROP INDEX IF EXISTS idx_cleanup_queue_backend_storage_key;
CREATE INDEX IF NOT EXISTS idx_cleanup_queue_backend_storage_key
    ON cleanup_queue(backend_name, storage_key, size_bytes);

DROP INDEX IF EXISTS idx_cleanup_dlq_backend_storage_key;
CREATE INDEX IF NOT EXISTS idx_cleanup_dlq_backend_storage_key
    ON cleanup_dlq(backend_name, storage_key, size_bytes);

DROP INDEX IF EXISTS idx_pending_objects_backend_storage_key;
CREATE INDEX IF NOT EXISTS idx_pending_objects_backend_storage_key
    ON pending_objects(backend_name, storage_key, size_bytes);
