-- -------------------------------------------------------------------------------
-- Indexes for Key Range Listings
--
-- Author: Alex Freidah
--
-- Prefix queries now state a byte range, not a LIKE pattern, because Postgres
-- only turns a LIKE into index bounds when it plans with the pattern's value. A
-- prepared statement moves to a generic plan, and the listing then read past the
-- prefix to the end of the index.
--
-- The listing index gains what the listing reads: object_size for the client
-- size and managed for the filter. Without them every replica row walked costs a
-- heap fetch. size_bytes stays because the directory roll-up sums it, and that
-- query falls back to a sequential scan without it.
--
-- Measured on 1.7M rows, listing a 1000-key page:
--
--     previous index    Index Scan, 2.2 ms
--     this index        Index Only Scan, no heap fetches, 169 buffers, 0.5 ms
--
-- The two text_pattern_ops indexes served LIKE and nothing else. A pattern
-- opclass cannot serve a COLLATE "C" comparison, so multipart_uploads gets a
-- C-collated index in its place, and object_locations already has the listing
-- index.
-- -------------------------------------------------------------------------------

-- +goose Up
-- +goose NO TRANSACTION

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_object_locations_listing
    ON object_locations (object_key COLLATE "C", created_at)
    INCLUDE (backend_name, size_bytes, object_size, etag, managed);

DROP INDEX CONCURRENTLY IF EXISTS idx_object_locations_key_collate_c_covering;

DROP INDEX CONCURRENTLY IF EXISTS idx_object_locations_key_pattern;

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_multipart_uploads_key_collate_c
    ON multipart_uploads (object_key COLLATE "C", created_at);

DROP INDEX CONCURRENTLY IF EXISTS idx_multipart_uploads_key_pattern;

-- +goose Down
-- +goose NO TRANSACTION

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_multipart_uploads_key_pattern
    ON multipart_uploads (object_key text_pattern_ops);

DROP INDEX CONCURRENTLY IF EXISTS idx_multipart_uploads_key_collate_c;

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_object_locations_key_pattern
    ON object_locations (object_key text_pattern_ops);

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_object_locations_key_collate_c_covering
    ON object_locations (object_key COLLATE "C", created_at)
    INCLUDE (backend_name, size_bytes, etag);

DROP INDEX CONCURRENTLY IF EXISTS idx_object_locations_listing;
