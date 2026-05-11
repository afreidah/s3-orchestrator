-- -----------------------------------------------------------------------------
-- Multipart Upload Encryption Metadata
--
-- Author: Alex Freidah
--
-- Adds upload-level encryption metadata to multipart_uploads so every part of a
-- given upload can share one wrapped DEK instead of each part independently
-- wrapping a fresh DEK against the KeyProvider. Concretely this turns N+1
-- WrapDEK round-trips per encrypted multipart upload into 1 (CreateMultipartUpload
-- wraps once, UploadPart unwraps from the upload row, CompleteMultipartUpload
-- reuses the same DEK for the assembled object).
--
-- Columns are nullable in the schema so the migration succeeds against a
-- database with in-flight uploads. New uploads always populate them on
-- insert via the runtime CreateMultipartUpload code path; any rows left
-- NULL by a prior version that lacked the columns will fail
-- CompleteMultipartUpload until manually resolved.
--
-- The packed format of encryption_key matches multipart_parts.encryption_key
-- and object_locations.encryption_key (base nonce || wrapped DEK) so the
-- existing encryption.UnpackKeyData / PackKeyData helpers apply unchanged.
-- -----------------------------------------------------------------------------

-- +goose Up
ALTER TABLE multipart_uploads
    ADD COLUMN encryption_key BYTEA,
    ADD COLUMN key_id         TEXT;

-- +goose Down
ALTER TABLE multipart_uploads
    DROP COLUMN IF EXISTS encryption_key,
    DROP COLUMN IF EXISTS key_id;
