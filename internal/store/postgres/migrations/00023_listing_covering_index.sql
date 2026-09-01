-- -------------------------------------------------------------------------------
-- Covering Index for the Prefix Listing
--
-- Author: Alex Freidah
--
-- ListObjectsByPrefix dedups replicas with DISTINCT ON, so it walks every copy
-- row per key and emits one. The previous index held only the key, which cost
-- the listing two things: a heap fetch per row walked to reach the projected
-- columns, and an incremental sort per key group to pick the DISTINCT ON winner
-- by created_at.
--
-- Carrying created_at as a second key column rather than in INCLUDE is what
-- removes the sort. INCLUDE columns are unordered payload: they satisfy the
-- projection but cannot satisfy an ORDER BY, so an index-only scan that keeps
-- created_at in the payload still sorts each group.
--
-- Measured on 360k rows (120k keys x 3 copies), listing a 1000-key page:
--
--     key only                          3017 buffers, Incremental Sort, 1.890 ms
--     key + INCLUDE(.., created_at)       51 buffers, Incremental Sort, 1.346 ms
--     (key, created_at) + INCLUDE(..)     51 buffers, no sort,         0.847 ms
--
-- This replaces idx_object_locations_key_collate_c rather than joining it. The
-- leading column is identical, so the delimiter listing's skip-scan plans an
-- index-only scan on this one too, and the index count on the table does not
-- grow. The entries are wider - roughly 45 MB against 10 MB at that row count -
-- which every write pays, and that is the cost being accepted for a listing
-- that no longer touches the heap.
--
-- idx_object_locations_key_created stays. The delimiter listing's LATERAL leaf
-- lookup joins on object_key in the database collation, which does not use a
-- C-collated index; that query plans on the default-collation index instead.
-- -------------------------------------------------------------------------------

-- +goose Up
-- +goose NO TRANSACTION

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_object_locations_key_collate_c_covering
    ON object_locations (object_key COLLATE "C", created_at)
    INCLUDE (backend_name, size_bytes, etag);

DROP INDEX CONCURRENTLY IF EXISTS idx_object_locations_key_collate_c;

-- +goose Down
-- +goose NO TRANSACTION

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_object_locations_key_collate_c
    ON object_locations (object_key COLLATE "C");

DROP INDEX CONCURRENTLY IF EXISTS idx_object_locations_key_collate_c_covering;
