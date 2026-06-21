-- -------------------------------------------------------------------------------
-- Byte-Order Index for Delimiter Prefix Listing
--
-- Author: Alex Freidah
--
-- ListObjectsDelimited folds keys into CommonPrefixes with a loose index scan
-- (recursive CTE skip-scan) that seeks group-to-group using
-- object_key COLLATE "C" in both the cursor predicate and ORDER BY, so grouping
-- and pagination follow S3 UTF-8 byte order and match SQLite's BINARY collation.
-- This index backs that access path; the default locale-collated
-- (object_key, created_at) index cannot satisfy a COLLATE "C" ordering, which
-- would force a per-step sort and defeat the skip scan.
-- -------------------------------------------------------------------------------

-- +goose Up
-- +goose NO TRANSACTION

-- Backs ListObjectsDelimited: WHERE object_key LIKE $prefix || '%'
-- AND object_key COLLATE "C" > $bound ORDER BY object_key COLLATE "C".
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_object_locations_key_collate_c
    ON object_locations (object_key COLLATE "C");

-- +goose Down
DROP INDEX IF EXISTS idx_object_locations_key_collate_c;
