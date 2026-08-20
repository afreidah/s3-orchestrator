-- Record how the bytes on a backend were compressed, so a read can undo it.
--
-- The algorithm is what a decoder dispatches on and the format version is what
-- makes a later change to the on-disk layout detectable rather than silently
-- misread; the level is diagnostic and tells a rewrite pass what an object was
-- written at.
--
-- logical_size is the size of the object the client wrote, which is distinct
-- from plaintext_size: with both compression and encryption on, the stored
-- bytes are ciphertext of compressed data, plaintext_size is the pre-encryption
-- (compressed) size, and logical_size is the original.
--
-- Nullable throughout, and a NULL algorithm means the bytes are stored
-- verbatim, which is what every pre-existing row is.
--
-- SQLite has no multi-column ADD COLUMN and no ADD COLUMN IF NOT EXISTS, so
-- each column is its own statement. The runner applies a migration once per
-- database, guarded by schema_version.

ALTER TABLE object_locations ADD COLUMN compression_algorithm TEXT;
ALTER TABLE object_locations ADD COLUMN compression_level TEXT;
ALTER TABLE object_locations ADD COLUMN compression_format_version INTEGER;
ALTER TABLE object_locations ADD COLUMN logical_size INTEGER;

ALTER TABLE pending_objects ADD COLUMN compression_algorithm TEXT;
ALTER TABLE pending_objects ADD COLUMN compression_level TEXT;
ALTER TABLE pending_objects ADD COLUMN compression_format_version INTEGER;
ALTER TABLE pending_objects ADD COLUMN logical_size INTEGER;
