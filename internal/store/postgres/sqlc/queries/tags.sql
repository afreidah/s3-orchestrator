-- -----------------------------------------------------------------------------
-- Object Tag Queries
--
-- Author: Alex Freidah
--
-- sqlc-input definitions for object_tags - the S3 object tag set, keyed by
-- object rather than by copy. Covers reading a set, replacing one (as a
-- delete plus per-tag insert composed in core), and the clears that run
-- wherever a key stops holding the object it held.
-- -----------------------------------------------------------------------------

-- name: GetObjectTags :many
-- Ordered by key so the Tagging XML response is byte-identical run to run.
-- An unordered result varies with physical row order and flakes the tests.
SELECT tag_key, tag_value
FROM object_tags
WHERE object_key = $1
ORDER BY tag_key;

-- name: CountObjectTags :one
-- Serves the tagging-count header on the read path, which needs the size of
-- the set rather than its contents. The object_tags primary key leads with
-- object_key, so this is an index-only scan over the one key's rows.
SELECT count(*)
FROM object_tags
WHERE object_key = $1;

-- name: InsertObjectTag :exec
-- Replace semantics are a delete followed by these inserts inside one
-- transaction, so a conflict here means the caller passed a duplicate key
-- and should surface as an error rather than being silently absorbed.
INSERT INTO object_tags (object_key, tag_key, tag_value)
VALUES ($1, $2, $3);

-- name: DeleteObjectTags :exec
DELETE FROM object_tags
WHERE object_key = $1;

-- name: DeleteObjectTagsForKeys :exec
-- Batch form for the multi-key delete path. One statement rather than a
-- statement per key, matching how DeleteObjectsByKeys removes the locations.
DELETE FROM object_tags
WHERE object_key = ANY(sqlc.arg(object_keys)::text[]);
