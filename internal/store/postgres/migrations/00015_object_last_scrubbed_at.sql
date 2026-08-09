-- -------------------------------------------------------------------------------
-- Last-Scrubbed Timestamp for Object Locations
--
-- Author: Alex Freidah
--
-- The scrubber selected its candidates at random, which gave no guarantee that
-- any particular copy would ever be verified. On Postgres it was worse than
-- unbounded: TABLESAMPLE walks the heap in physical order and LIMIT halts the
-- scan once the batch is full, so the scan never travelled past the first few
-- hundred pages and the rest of the table was unreachable.
--
-- Recording when each copy was last checked turns the sweep into an ordered
-- queue: oldest first, never-checked ahead of everything. Coverage stops being
-- probabilistic, and the age of the least recently verified copy becomes a
-- number an operator can alert on.
--
-- Nullable, because every pre-existing row is genuinely unverified and belongs
-- at the front of the queue rather than being backdated to now.
-- -------------------------------------------------------------------------------

-- +goose Up
ALTER TABLE object_locations ADD COLUMN last_scrubbed_at TIMESTAMPTZ;

-- Matches the scrub candidate query exactly: the partial predicate keeps the
-- index to the rows the scrubber can act on, and NULLS FIRST matches the sort
-- so never-verified copies are read straight off the front.
CREATE INDEX IF NOT EXISTS idx_object_locations_scrub_queue
    ON object_locations (last_scrubbed_at NULLS FIRST, object_key)
    WHERE content_hash IS NOT NULL AND managed;

-- +goose Down
DROP INDEX IF EXISTS idx_object_locations_scrub_queue;
ALTER TABLE object_locations DROP COLUMN IF EXISTS last_scrubbed_at;
