-- -------------------------------------------------------------------------------
-- Add Orphan Bytes Tracking
--
-- Author: Alex Freidah
--
-- Tracks bytes that have been logically freed (quota decremented) but not yet
-- physically deleted from the backend. Prevents the write path from
-- overcommitting storage when backends are temporarily unreachable.
-- -------------------------------------------------------------------------------

-- +goose Up
ALTER TABLE backend_quotas ADD COLUMN orphan_bytes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE cleanup_queue ADD COLUMN size_bytes BIGINT NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE backend_quotas DROP COLUMN orphan_bytes;
ALTER TABLE cleanup_queue DROP COLUMN size_bytes;
