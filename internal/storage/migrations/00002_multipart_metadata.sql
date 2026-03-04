-- +goose Up
ALTER TABLE multipart_uploads ADD COLUMN metadata JSONB;

-- +goose Down
ALTER TABLE multipart_uploads DROP COLUMN IF EXISTS metadata;
