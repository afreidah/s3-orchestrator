-- The client-facing identity of an object, held next to the copy rather than
-- fetched from whichever backend answers. etag is the MD5 of the bytes the
-- client wrote (or the AWS composite for a multipart upload), so every copy
-- reports the same validator and a read that fails over to a replica does not
-- change what a conditional request compares against.
--
-- content_type and user_metadata are the rest of what a HEAD answers with:
-- with all three present the response comes from this table and the backend
-- round trip is skipped.
--
-- NULL means unknown, which is what every pre-existing row is. That is
-- distinct from a known-empty value: '' is a content type the client set and
-- '{}' is an object known to carry no user metadata.
--
-- pending_objects carries the same columns so a reaper-promoted intent keeps
-- the identity the write already knew.
--
-- multipart_parts.plaintext_etag is the MD5 of the bytes the client sent for
-- the part, which the existing etag column is not: that holds what the backend
-- returned for the part as stored, a digest of ciphertext once encryption is
-- on. The AWS multipart ETag is built from the plaintext digests.

ALTER TABLE object_locations ADD COLUMN etag          TEXT;
ALTER TABLE object_locations ADD COLUMN content_type  TEXT;
ALTER TABLE object_locations ADD COLUMN user_metadata TEXT;

ALTER TABLE pending_objects ADD COLUMN etag          TEXT;
ALTER TABLE pending_objects ADD COLUMN content_type  TEXT;
ALTER TABLE pending_objects ADD COLUMN user_metadata TEXT;

ALTER TABLE multipart_parts ADD COLUMN plaintext_etag TEXT;
