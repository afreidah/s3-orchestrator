-- -------------------------------------------------------------------------------
-- Compression Probe Result for Declined Objects
--
-- Author: Alex Freidah
--
-- Records what the encoder produced for a copy it declined to store compressed,
-- so a later pass can reach the same verdict without downloading and encoding
-- the object again. An incompressible copy is metered egress every time it is
-- rediscovered, and compress-existing walks the whole fleet.
--
-- The measurement is stored rather than a declined flag, because the verdict
-- depends on configuration the measurement does not: min_ratio is applied to
-- compression_probe_size at query time, so loosening it returns those copies to
-- the pass with no read at all. A flag would have to be found and cleared.
--
-- compression_probe_level names the level the size was measured at, matching
-- compression_level's type. A probe taken at a different level says nothing
-- about the current one, so the listing ignores it rather than comparing the
-- two: the levels are names from an ordered set, not numbers, and ordering them
-- in SQL would order them alphabetically.
--
-- Nullable, and NULL means the copy has never been probed. That is what every
-- pre-existing row is, so no backfill is needed. Only the min_ratio decline
-- writes here: a min_size decline is answered from the row at no cost, and a
-- copy declined by usage limits never reached the encoder.
-- -------------------------------------------------------------------------------

-- +goose Up

ALTER TABLE object_locations
    ADD COLUMN compression_probe_size  BIGINT,
    ADD COLUMN compression_probe_level TEXT;

-- +goose Down

ALTER TABLE object_locations
    DROP COLUMN IF EXISTS compression_probe_size,
    DROP COLUMN IF EXISTS compression_probe_level;
