-- Drain and purge page through a backend's managed rows smallest first, by
-- (size_bytes, object_key), which the managed index could filter but not order.
-- Reconcile and the cleanup sweep look up queued and dead-lettered deletes by
-- path, which neither table indexed. The dropped indexes are covered by the
-- leading column of a wider one.

CREATE INDEX IF NOT EXISTS idx_object_locations_managed_size
    ON object_locations(backend_name, size_bytes, object_key) WHERE managed;

CREATE INDEX IF NOT EXISTS idx_cleanup_queue_backend_storage_key
    ON cleanup_queue(backend_name, storage_key);

CREATE INDEX IF NOT EXISTS idx_cleanup_dlq_backend_storage_key
    ON cleanup_dlq(backend_name, storage_key);

DROP INDEX IF EXISTS idx_object_locations_managed;
DROP INDEX IF EXISTS idx_object_locations_backend;
DROP INDEX IF EXISTS idx_cleanup_dlq_backend;
