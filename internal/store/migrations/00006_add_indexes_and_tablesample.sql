-- -------------------------------------------------------------------------------
-- Add Missing Indexes and Replace Random Scrubber Query
--
-- Author: Alex Freidah
--
-- Adds two performance-critical indexes:
-- 1. multipart_uploads(backend_name) — eliminates full table scan in the inflight
--    size subquery executed on every write-path quota check.
-- 2. object_locations(object_key, created_at) — accelerates DISTINCT ON sorting
--    for ListObjects and directory tree queries.
-- -------------------------------------------------------------------------------

-- +goose Up
-- +goose NO TRANSACTION

-- Accelerate the inflight multipart size calculation in quota queries.
-- Without this index, GetBackendAvailableSpace and GetLeastUtilizedBackend
-- perform a sequential scan of multipart_uploads on every write.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_multipart_uploads_backend_name
    ON multipart_uploads(backend_name);

-- Accelerate DISTINCT ON (object_key) ... ORDER BY object_key, created_at
-- used by ListObjectsByPrefix, GetDirectoryStats, and other listing queries.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_object_locations_key_created
    ON object_locations(object_key, created_at);

-- +goose Down
DROP INDEX IF EXISTS idx_multipart_uploads_backend_name;
DROP INDEX IF EXISTS idx_object_locations_key_created;
