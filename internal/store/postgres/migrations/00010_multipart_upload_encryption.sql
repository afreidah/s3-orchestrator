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
-- database with in-flight uploads. The application's startup path runs a
-- backfill worker (internal/worker/multipart_dek_backfill.go) that fetches
-- each legacy upload's parts, re-encrypts them under a freshly-generated
-- upload-level DEK via temp-key swap, and populates these columns. After the
-- backfill completes every row is non-null; the runtime CreateMultipartUpload
-- code path always populates them on insert.
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
