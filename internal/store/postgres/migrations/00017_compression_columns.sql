-- -------------------------------------------------------------------------------
-- Compression Metadata for Stored Objects
--
-- Author: Alex Freidah
--
-- Records how the bytes on a backend were compressed, so a read can undo it.
-- The algorithm is what a decoder dispatches on and the format version is what
-- makes a later change to the on-disk layout detectable rather than silently
-- misread; the level is diagnostic and tells a rewrite pass what an object was
-- written at.
--
-- logical_size is the size of the object the client wrote, which is distinct
-- from plaintext_size: with both compression and encryption on, the stored
-- bytes are ciphertext of compressed data, plaintext_size is the pre-encryption
-- (compressed) size, and logical_size is the original. Both are needed to size
-- a response and bound range math.
--
-- Nullable throughout, and a NULL algorithm means the bytes are stored
-- verbatim. That is what every pre-existing row is, so no backfill is needed
-- and no separate boolean can drift out of step with the algorithm.
--
-- The same columns land on pending_objects because an intent records what was
-- written to the backend before the commit; a reaper promoting one has to
-- carry the representation forward or the promoted row describes bytes that
-- are not there.
-- -------------------------------------------------------------------------------

-- +goose Up

ALTER TABLE object_locations
    ADD COLUMN compression_algorithm      TEXT,
    ADD COLUMN compression_level          TEXT,
    ADD COLUMN compression_format_version SMALLINT,
    ADD COLUMN logical_size               BIGINT;

ALTER TABLE pending_objects
    ADD COLUMN compression_algorithm      TEXT,
    ADD COLUMN compression_level          TEXT,
    ADD COLUMN compression_format_version SMALLINT,
    ADD COLUMN logical_size               BIGINT;

-- +goose Down

ALTER TABLE pending_objects
    DROP COLUMN IF EXISTS compression_algorithm,
    DROP COLUMN IF EXISTS compression_level,
    DROP COLUMN IF EXISTS compression_format_version,
    DROP COLUMN IF EXISTS logical_size;

ALTER TABLE object_locations
    DROP COLUMN IF EXISTS compression_algorithm,
    DROP COLUMN IF EXISTS compression_level,
    DROP COLUMN IF EXISTS compression_format_version,
    DROP COLUMN IF EXISTS logical_size;
