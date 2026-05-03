-- -----------------------------------------------------------------------------
-- Multipart Metadata
--
-- Author: Alex Freidah
--
-- Adds the JSONB metadata column to multipart_uploads so user-supplied
-- x-amz-meta-* headers attached to CreateMultipartUpload survive across
-- the upload window and apply to the assembled object on Complete.
-- -----------------------------------------------------------------------------

-- +goose Up
ALTER TABLE multipart_uploads ADD COLUMN metadata JSONB;

-- +goose Down
ALTER TABLE multipart_uploads DROP COLUMN IF EXISTS metadata;
