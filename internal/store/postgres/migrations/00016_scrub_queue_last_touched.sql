-- -------------------------------------------------------------------------------
-- Scrub Queue Ordered by Last Touch
--
-- Project: s3-orchestrator / Author: Alex Freidah
-- -------------------------------------------------------------------------------
--
-- Sorting never-verified copies first starves the sweep on a write-heavy fleet:
-- new copies arrive as NULL at the head of the queue, and once they arrive
-- faster than the scrubber consumes them the sweep never advances past objects
-- written in the last few hours. It also inverts the priority, since churn is
-- compacted away long before it can degrade while the copies that persist for
-- months are the ones bit rot reaches.
--
-- Falling back to created_at puts a freshly written copy at the back of the
-- queue and leaves an old unverified one at the front, so the write rate stops
-- competing with coverage.
--
-- COALESCE over two immutable timestamp columns is itself immutable, so the
-- expression can be indexed and the candidate query still avoids a sort.
-- -------------------------------------------------------------------------------

-- +goose Up
DROP INDEX IF EXISTS idx_object_locations_scrub_queue;

CREATE INDEX IF NOT EXISTS idx_object_locations_scrub_queue
    ON object_locations (COALESCE(last_scrubbed_at, created_at), object_key)
    WHERE content_hash IS NOT NULL AND managed;

-- +goose Down
DROP INDEX IF EXISTS idx_object_locations_scrub_queue;

CREATE INDEX IF NOT EXISTS idx_object_locations_scrub_queue
    ON object_locations (last_scrubbed_at NULLS FIRST, object_key)
    WHERE content_hash IS NOT NULL AND managed;
