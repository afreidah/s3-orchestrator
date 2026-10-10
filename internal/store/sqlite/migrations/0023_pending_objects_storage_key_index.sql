-- Composite index on pending_objects(backend_name, storage_key) to accelerate
-- ListedPathStates intent lookups. The old idx_pending_objects_backend index
-- is covered by the new composite index's leading column and is dropped.

CREATE INDEX IF NOT EXISTS idx_pending_objects_backend_storage_key
    ON pending_objects(backend_name, storage_key);

DROP INDEX IF EXISTS idx_pending_objects_backend;
