-- -------------------------------------------------------------------------------
-- Object Identity: ETag, Content Type and User Metadata
--
-- Author: Alex Freidah
--
-- The client-facing identity of an object, held next to the copy rather than
-- fetched from whichever backend answers. etag is the MD5 of the bytes the
-- client wrote (or the AWS composite for a multipart upload), so every copy
-- reports the same validator and a read that fails over to a replica does not
-- change what a conditional request compares against.
--
-- content_type and user_metadata ride along because they are the rest of what
-- a HEAD answers with: with all three present the response is served from this
-- table and the backend round trip - a metered call - is skipped entirely.
--
-- NULL means unknown, which is what every pre-existing row is and what an
-- object imported by reconcile stays until something reads it. That is
-- distinct from a known-empty value: '' is a content type the client set and
-- '{}' is an object known to carry no user metadata, and both are answerable
-- without asking the backend.
--
-- The same columns land on pending_objects because an intent records what was
-- written before the commit, and a reaper promoting one carries the identity
-- forward instead of producing a row that has to be re-learned.
--
-- multipart_parts.plaintext_etag is the MD5 of the bytes the client sent for
-- that part, which the existing etag column is not: that one holds what the
-- backend returned for the part as stored, which is a digest of ciphertext
-- once encryption is on. The AWS multipart ETag is the MD5 of the concatenated
-- part digests, so the composite can only be built from plaintext ones.
-- -------------------------------------------------------------------------------

-- +goose Up

ALTER TABLE object_locations
    ADD COLUMN etag          TEXT,
    ADD COLUMN content_type  TEXT,
    ADD COLUMN user_metadata JSONB;

ALTER TABLE pending_objects
    ADD COLUMN etag          TEXT,
    ADD COLUMN content_type  TEXT,
    ADD COLUMN user_metadata JSONB;

ALTER TABLE multipart_parts
    ADD COLUMN plaintext_etag TEXT;

-- +goose Down

ALTER TABLE multipart_parts
    DROP COLUMN IF EXISTS plaintext_etag;

ALTER TABLE pending_objects
    DROP COLUMN IF EXISTS etag,
    DROP COLUMN IF EXISTS content_type,
    DROP COLUMN IF EXISTS user_metadata;

ALTER TABLE object_locations
    DROP COLUMN IF EXISTS etag,
    DROP COLUMN IF EXISTS content_type,
    DROP COLUMN IF EXISTS user_metadata;
