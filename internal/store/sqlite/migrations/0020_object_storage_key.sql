-- The path a copy's bytes occupy on its backend, recorded next to the copy
-- instead of being assumed to equal the object's key.
--
-- Every write of a key used to land at the key itself, so two overwrites racing
-- towards one backend wrote the same path and the cleanup that followed the
-- upload which lost the race deleted whichever version was sitting there - see
-- issue #1527. A write now stores its bytes under object_key || '!' ||
-- intent_id and the row points at that path, so a discard deletes what it
-- wrote, an overwrite never writes in place, and every queued deletion says
-- which bytes it is for.
--
-- pending_objects carries it because the intent is written before the upload
-- and is the only record of where the bytes went if the commit never happens.
-- cleanup_queue and cleanup_dlq carry it because a queued deletion outlives the
-- row it came from; object_key stays beside it so the queue still says which
-- object an orphan belongs to.
--
-- Existing rows take storage_key = object_key, which is where their bytes
-- actually are. SQLite can only add a NOT NULL column with a default, so the
-- column is added with '' and then filled; every insert supplies a value, so
-- the default is never what a row ends up holding.

ALTER TABLE object_locations ADD COLUMN storage_key TEXT NOT NULL DEFAULT '';
UPDATE object_locations SET storage_key = object_key WHERE storage_key = '';

ALTER TABLE pending_objects ADD COLUMN storage_key TEXT NOT NULL DEFAULT '';
UPDATE pending_objects SET storage_key = object_key WHERE storage_key = '';

ALTER TABLE cleanup_queue ADD COLUMN storage_key TEXT NOT NULL DEFAULT '';
UPDATE cleanup_queue SET storage_key = object_key WHERE storage_key = '';

ALTER TABLE cleanup_dlq ADD COLUMN storage_key TEXT NOT NULL DEFAULT '';
UPDATE cleanup_dlq SET storage_key = object_key WHERE storage_key = '';

-- One index serves both jobs. Reconcile's sorted-merge join walks the ledger by
-- storage_key now, because that is what a backend listing returns, and SQLite
-- compares TEXT in byte order by default so the index is already in the order
-- the merge needs. Being unique also states the invariant the whole change
-- exists to create: one backend holds one object at one path.
CREATE UNIQUE INDEX IF NOT EXISTS idx_object_locations_backend_storage_key
    ON object_locations(backend_name, storage_key);
