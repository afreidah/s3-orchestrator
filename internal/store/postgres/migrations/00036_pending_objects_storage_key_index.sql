-- -------------------------------------------------------------------------------
-- Index for ListedPathStates pending_objects lookup
--
-- Author: Adam Magued
--
-- ListedPathStates checks pending_objects by (backend_name, storage_key) to
-- detect in-flight PUT intents during sync reconcile and import checks.
-- idx_pending_objects_backend is dropped because (backend_name, storage_key)
-- subsumes it.
-- -------------------------------------------------------------------------------

-- +goose Up
-- +goose NO TRANSACTION

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_pending_objects_backend_storage_key
    ON pending_objects (backend_name, storage_key);

DROP INDEX CONCURRENTLY IF EXISTS idx_pending_objects_backend;

-- +goose Down
-- +goose NO TRANSACTION

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_pending_objects_backend
    ON pending_objects (backend_name);

DROP INDEX CONCURRENTLY IF EXISTS idx_pending_objects_backend_storage_key;
