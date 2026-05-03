-- -----------------------------------------------------------------------------
-- Server-Side Envelope Encryption Columns
--
-- Author: Alex Freidah
--
-- Adds the per-row encryption fields (encrypted, encryption_key, key_id,
-- plaintext_size) to object_locations and multipart_parts so each row
-- carries everything decryption needs without an external lookup. The new
-- columns default to a non-encrypted-equivalent value so pre-existing rows
-- stay valid after migration.
-- -----------------------------------------------------------------------------

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
