-- +goose Up
ALTER TABLE object_locations
    ADD COLUMN encrypted      BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN encryption_key BYTEA,
    ADD COLUMN key_id         TEXT,
    ADD COLUMN plaintext_size BIGINT;

ALTER TABLE multipart_parts
    ADD COLUMN encrypted      BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN encryption_key BYTEA,
    ADD COLUMN key_id         TEXT,
    ADD COLUMN plaintext_size BIGINT;

-- +goose Down
ALTER TABLE object_locations
    DROP COLUMN IF EXISTS encrypted,
    DROP COLUMN IF EXISTS encryption_key,
    DROP COLUMN IF EXISTS key_id,
    DROP COLUMN IF EXISTS plaintext_size;

ALTER TABLE multipart_parts
    DROP COLUMN IF EXISTS encrypted,
    DROP COLUMN IF EXISTS encryption_key,
    DROP COLUMN IF EXISTS key_id,
    DROP COLUMN IF EXISTS plaintext_size;
