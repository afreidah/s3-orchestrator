-- +goose Up
ALTER TABLE object_locations ADD COLUMN content_hash TEXT;

-- +goose Down
ALTER TABLE object_locations DROP COLUMN IF EXISTS content_hash;
