-- -------------------------------------------------------------------------------
-- Per-Write Storage Keys
--
-- Author: Alex Freidah
--
-- The path a copy's bytes occupy on its backend, recorded next to the copy
-- instead of being assumed to equal the object's key.
--
-- Every write of a key used to land at the key itself. That is what issue #1527
-- is: two overwrites racing towards one slow backend wrote the same path, and
-- the cleanup that followed the companion upload which lost the race deleted
-- "the object at that key" - which by then was the winner's. The ledger kept a
-- row naming a copy whose bytes were gone, the replication factor was silently
-- one short, and nothing but the scrubber could notice.
--
-- A write now stores its bytes under object_key || '!' || intent_id, and the
-- row points at that path. A discard deletes the object it wrote and no other;
-- an overwrite never writes in place, so a read during one sees the old object
-- or the new one rather than a path being mutated underneath it; and every
-- queued deletion says which bytes it is for.
--
-- pending_objects carries the column for the same reason it carries the stored
-- form: the intent is written before the upload, so it is the only record of
-- where the bytes went if the commit never happens. The reaper and the
-- companion-discard path both delete at what it says.
--
-- cleanup_queue and cleanup_dlq carry it because a queued deletion outlives
-- everything that knew the path: the row it came from is already gone by the
-- time the worker runs. object_key stays alongside it so an operator reading
-- the queue still sees which object the orphan belongs to.
--
-- The backfill is storage_key = object_key, which is where a row written before
-- this migration actually has its bytes. Nothing downstream has to recognise
-- those rows as special - they are read through the same column as every other.
-- -------------------------------------------------------------------------------

-- +goose Up

ALTER TABLE object_locations ADD COLUMN storage_key TEXT;
UPDATE object_locations SET storage_key = object_key WHERE storage_key IS NULL;
ALTER TABLE object_locations ALTER COLUMN storage_key SET NOT NULL;

ALTER TABLE pending_objects ADD COLUMN storage_key TEXT;
UPDATE pending_objects SET storage_key = object_key WHERE storage_key IS NULL;
ALTER TABLE pending_objects ALTER COLUMN storage_key SET NOT NULL;

ALTER TABLE cleanup_queue ADD COLUMN storage_key TEXT;
UPDATE cleanup_queue SET storage_key = object_key WHERE storage_key IS NULL;
ALTER TABLE cleanup_queue ALTER COLUMN storage_key SET NOT NULL;

ALTER TABLE cleanup_dlq ADD COLUMN storage_key TEXT;
UPDATE cleanup_dlq SET storage_key = object_key WHERE storage_key IS NULL;
ALTER TABLE cleanup_dlq ALTER COLUMN storage_key SET NOT NULL;

-- +goose Down

ALTER TABLE cleanup_dlq DROP COLUMN IF EXISTS storage_key;
ALTER TABLE cleanup_queue DROP COLUMN IF EXISTS storage_key;
ALTER TABLE pending_objects DROP COLUMN IF EXISTS storage_key;
ALTER TABLE object_locations DROP COLUMN IF EXISTS storage_key;
