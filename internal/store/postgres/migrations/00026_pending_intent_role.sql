-- +goose Up
-- -----------------------------------------------------------------------------
-- Pending Intent Role
--
-- Author: Alex Freidah
--
-- An intent has meant one thing until now: this write replaces whatever the key
-- held, so promoting it clears the key's other copies. A write that places the
-- object on several backends at once needs the other meaning as well - this
-- write adds a copy - because promoting one of its intents must not delete the
-- copies its siblings committed.
--
-- Existing rows are primary, which is what every intent written before this
-- migration meant.
-- -----------------------------------------------------------------------------

ALTER TABLE pending_objects
    ADD COLUMN role TEXT NOT NULL DEFAULT 'primary'
    CONSTRAINT pending_objects_role_check CHECK (role IN ('primary', 'companion'));

-- Every write now clears the leftover intents for the key it is committing, so
-- the table is read by object_key on the write path rather than only by the
-- reaper's created_at scan.
CREATE INDEX IF NOT EXISTS idx_pending_objects_key
    ON pending_objects (object_key);

-- +goose Down
DROP INDEX IF EXISTS idx_pending_objects_key;

ALTER TABLE pending_objects DROP COLUMN role;
