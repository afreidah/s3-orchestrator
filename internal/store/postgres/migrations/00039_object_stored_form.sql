-- -----------------------------------------------------------------------------
-- Stored Form Columns
--
-- Author: Alex Freidah
--
-- Two columns generated from the ones they summarize, so neither can disagree
-- with them.
--
-- form is the set of layers a copy's bytes carry: compressed (1), encrypted
-- (2), hashed (4) and keyed (8), keyed meaning the row holds the key that opens
-- its envelope. Bits are only ever added, never renumbered. Queries test it
-- against masks the store passes in, so what each bit means is defined once,
-- in Go.
--
-- object_size is the size of the object the client wrote: logical_size for an
-- encoded copy, plaintext_size for an encrypted one, and size_bytes otherwise.
--
-- An empty string counts as absent, the same as NULL, which is how Go reads
-- both. Adding stored generated columns rewrites object_locations under an
-- exclusive lock.
-- -----------------------------------------------------------------------------

-- +goose Up
ALTER TABLE object_locations
    ADD COLUMN form BIGINT NOT NULL GENERATED ALWAYS AS (
        (CASE WHEN NULLIF(compression_algorithm, '') IS NOT NULL THEN 1 ELSE 0 END)
        | (CASE WHEN encrypted THEN 2 ELSE 0 END)
        | (CASE WHEN NULLIF(content_hash, '') IS NOT NULL THEN 4 ELSE 0 END)
        | (CASE WHEN length(encryption_key) > 0 THEN 8 ELSE 0 END)
    ) STORED,
    ADD COLUMN object_size BIGINT NOT NULL GENERATED ALWAYS AS (
        CASE
            WHEN NULLIF(compression_algorithm, '') IS NOT NULL THEN COALESCE(logical_size, 0)
            WHEN encrypted THEN COALESCE(plaintext_size, 0)
            ELSE size_bytes
        END
    ) STORED;

-- +goose Down
ALTER TABLE object_locations
    DROP COLUMN object_size,
    DROP COLUMN form;
