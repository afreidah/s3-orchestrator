-- -------------------------------------------------------------------------------
-- Per-Write Storage Keys
--
-- Author: Alex Freidah
--
-- Records the path each copy's bytes occupy on its backend. A write stores its
-- bytes under object_key || '!' || an id of its own, so two writes to one key
-- never share a path and deleting one write's bytes cannot remove another's.
--
-- pending_objects carries the column because the intent is the only record of
-- where an upload's bytes went if its commit never happens. cleanup_queue and
-- cleanup_dlq carry it because a queued delete outlives the row it came from.
--
-- Existing rows are backfilled with their object key, which is where their
-- bytes already are, so nothing on any backend moves.
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

-- Dropping the column loses the path of every copy written since the upgrade:
-- its row would point at the object key, where there are no bytes. The
-- rollback refuses while any row is stored anywhere other than its object key.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM object_locations WHERE storage_key <> object_key)
       OR EXISTS (SELECT 1 FROM pending_objects WHERE storage_key <> object_key)
       OR EXISTS (SELECT 1 FROM cleanup_queue WHERE storage_key <> object_key)
       OR EXISTS (SELECT 1 FROM cleanup_dlq WHERE storage_key <> object_key)
    THEN
        RAISE EXCEPTION 'rows are stored under per-write storage keys; rolling back would lose their paths';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE cleanup_dlq DROP COLUMN IF EXISTS storage_key;
ALTER TABLE cleanup_queue DROP COLUMN IF EXISTS storage_key;
ALTER TABLE pending_objects DROP COLUMN IF EXISTS storage_key;
ALTER TABLE object_locations DROP COLUMN IF EXISTS storage_key;
