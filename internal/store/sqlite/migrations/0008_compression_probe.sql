-- Record what the encoder produced for a copy it declined to store compressed,
-- so a later pass reaches the same verdict without downloading and encoding the
-- object again. An incompressible copy is metered egress every time it is
-- rediscovered, and compress-existing walks the whole fleet.
--
-- The measurement is stored rather than a declined flag, because the verdict
-- depends on configuration the measurement does not: min_ratio is applied to
-- compression_probe_size at query time, so loosening it returns those copies to
-- the pass with no read at all. A flag would have to be found and cleared.
--
-- compression_probe_level names the level the size was measured at. A probe
-- taken at a different level says nothing about the current one, so the listing
-- ignores it rather than comparing the two: the levels are names from an
-- ordered set, not numbers.
--
-- Nullable, and NULL means never probed, which is what every pre-existing row
-- is. Only the min_ratio decline writes here: a min_size decline is answered
-- from the row, and a copy declined by usage limits never reached the encoder.
--
-- SQLite has no multi-column ADD COLUMN and no ADD COLUMN IF NOT EXISTS, so
-- each column is its own statement. The runner applies a migration once per
-- database, guarded by schema_version.

ALTER TABLE object_locations ADD COLUMN compression_probe_size INTEGER;
ALTER TABLE object_locations ADD COLUMN compression_probe_level TEXT;
