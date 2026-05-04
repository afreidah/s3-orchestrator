-- -----------------------------------------------------------------------------
-- Content Hash Column
--
-- Author: Alex Freidah
--
-- Adds the nullable content_hash column to object_locations for the
-- integrity verification feature. Nullable so pre-existing rows remain
-- valid; the backfill admin command computes and stores hashes for rows
-- that predate this migration.
-- -----------------------------------------------------------------------------

-- +goose Up
ALTER TABLE object_locations ADD COLUMN content_hash TEXT;

-- +goose Down
ALTER TABLE object_locations DROP COLUMN IF EXISTS content_hash;
