-- -------------------------------------------------------------------------------
-- Byte-Order Index for the Reconcile Cursor
--
-- Author: Alex Freidah
--
-- ListObjectsByBackendKeyAsc drives the reconcile sorted-merge join, which must
-- walk the DB cursor in the same order as S3 ListObjectsV2 (UTF-8 byte order)
-- and the Go merge's byte comparison. The query now uses object_key COLLATE "C"
-- in both the cursor predicate and ORDER BY; this index backs that access path
-- so paging stays index-ordered instead of falling back to a sort. The default
-- (locale-collated) index cannot satisfy a COLLATE "C" ordering.
-- -------------------------------------------------------------------------------

-- +goose Up
-- +goose NO TRANSACTION

-- Backs ListObjectsByBackendKeyAsc: WHERE backend_name = $1
-- AND object_key COLLATE "C" > $2 ORDER BY object_key COLLATE "C" ASC.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_object_locations_backend_key_collate_c
    ON object_locations (backend_name, object_key COLLATE "C");

-- +goose Down
DROP INDEX IF EXISTS idx_object_locations_backend_key_collate_c;
