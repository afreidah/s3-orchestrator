-- Records the path each copy's bytes occupy on its backend. A write stores its
-- bytes under object_key || '!' || an id of its own, so two writes to one key
-- never share a path and deleting one write's bytes cannot remove another's.
--
-- pending_objects carries the column because the intent is the only record of
-- where an upload's bytes went if its commit never happens. cleanup_queue and
-- cleanup_dlq carry it because a queued delete outlives the row it came from.
--
-- Existing rows are backfilled with their object key, which is where their
-- bytes already are. SQLite can only add a NOT NULL column with a default, so
-- the column is added with '' and then filled; every insert supplies a value.

ALTER TABLE object_locations ADD COLUMN storage_key TEXT NOT NULL DEFAULT '';
UPDATE object_locations SET storage_key = object_key WHERE storage_key = '';

ALTER TABLE pending_objects ADD COLUMN storage_key TEXT NOT NULL DEFAULT '';
UPDATE pending_objects SET storage_key = object_key WHERE storage_key = '';

ALTER TABLE cleanup_queue ADD COLUMN storage_key TEXT NOT NULL DEFAULT '';
UPDATE cleanup_queue SET storage_key = object_key WHERE storage_key = '';

ALTER TABLE cleanup_dlq ADD COLUMN storage_key TEXT NOT NULL DEFAULT '';
UPDATE cleanup_dlq SET storage_key = object_key WHERE storage_key = '';

-- Reconcile walks the ledger by storage_key, and SQLite compares TEXT in byte
-- order, so this index is already in the order the walk needs. Being unique,
-- it also enforces one object per path per backend.
CREATE UNIQUE INDEX IF NOT EXISTS idx_object_locations_backend_storage_key
    ON object_locations(backend_name, storage_key);
