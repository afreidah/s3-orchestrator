-- Two columns generated from the ones they summarize, so neither can disagree
-- with them. form is the set of layers a copy's bytes carry: compressed (1),
-- encrypted (2), hashed (4) and keyed (8). object_size is the size of the
-- object the client wrote. Virtual, since SQLite adds only virtual generated
-- columns to an existing table; they are computed when read.

ALTER TABLE object_locations ADD COLUMN form INTEGER NOT NULL GENERATED ALWAYS AS (
    (CASE WHEN NULLIF(compression_algorithm, '') IS NOT NULL THEN 1 ELSE 0 END)
    | (CASE WHEN encrypted THEN 2 ELSE 0 END)
    | (CASE WHEN NULLIF(content_hash, '') IS NOT NULL THEN 4 ELSE 0 END)
    | (CASE WHEN length(encryption_key) > 0 THEN 8 ELSE 0 END)
) VIRTUAL;

ALTER TABLE object_locations ADD COLUMN object_size INTEGER NOT NULL GENERATED ALWAYS AS (
    CASE
        WHEN NULLIF(compression_algorithm, '') IS NOT NULL THEN COALESCE(logical_size, 0)
        WHEN encrypted THEN COALESCE(plaintext_size, 0)
        ELSE size_bytes
    END
) VIRTUAL;
