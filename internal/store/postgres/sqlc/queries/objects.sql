-- name: LockObjectKeyForWrite :exec
SELECT pg_advisory_xact_lock(hashtext($1));

-- name: GetExistingCopiesForUpdate :many
SELECT backend_name, size_bytes, created_at
FROM object_locations
WHERE object_key = $1
FOR UPDATE;

-- name: DeleteObjectCopies :exec
DELETE FROM object_locations
WHERE object_key = $1;

-- name: InsertObjectLocation :exec
INSERT INTO object_locations (object_key, backend_name, size_bytes, encrypted, encryption_key, key_id, plaintext_size, content_hash, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW());

-- name: ListObjectsByBackend :many
SELECT object_key, backend_name, size_bytes, created_at
FROM object_locations
WHERE backend_name = $1
ORDER BY size_bytes ASC
LIMIT $2;

-- ListObjectsByBackendKeyAsc returns rows for a backend in ascending object_key
-- order, starting strictly after the supplied cursor. Used by ReconcileBackend
-- to drive a bounded-memory sorted-merge join against an S3 ListObjects walk.
-- Pass '' as the cursor on the first call.
-- name: ListObjectsByBackendKeyAsc :many
SELECT object_key, backend_name, size_bytes, created_at
FROM object_locations
WHERE backend_name = $1 AND object_key > $2
ORDER BY object_key ASC
LIMIT $3;

-- name: CheckObjectExistsOnBackend :one
SELECT EXISTS(
    SELECT 1 FROM object_locations
    WHERE object_key = $1 AND backend_name = $2
) AS exists;

-- name: LockObjectOnBackend :one
SELECT size_bytes, encrypted, encryption_key, key_id, plaintext_size, content_hash
FROM object_locations
WHERE object_key = $1 AND backend_name = $2
FOR UPDATE;

-- name: DeleteObjectFromBackend :exec
DELETE FROM object_locations
WHERE object_key = $1 AND backend_name = $2;

-- name: ListObjectsByPrefix :many
SELECT DISTINCT ON (object_key) object_key, backend_name, size_bytes, created_at
FROM object_locations
WHERE object_key LIKE @prefix::text || '%' ESCAPE '\'
  AND object_key > @start_after
ORDER BY object_key, created_at ASC
LIMIT @max_keys;

-- name: GetAllObjectLocations :many
SELECT object_key, backend_name, size_bytes, encrypted, encryption_key, key_id, plaintext_size, content_hash, created_at
FROM object_locations
WHERE object_key = $1
ORDER BY created_at ASC;

-- name: InsertObjectLocationIfNotExists :one
INSERT INTO object_locations (object_key, backend_name, size_bytes, encrypted, encryption_key, key_id, plaintext_size, content_hash, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
ON CONFLICT (object_key, backend_name) DO NOTHING
RETURNING true AS inserted;

-- name: GetDirectoryStats :many
-- Aggregate count and size for immediate children of a directory prefix.
-- Directories (containing a '/') and files are distinguished by is_dir.
-- file_count counts distinct object_keys (logical files); total_size sums
-- every replica row in object_locations so directory totals reflect real
-- physical storage consumption, matching the Storage Summary semantics.
SELECT
    (CASE WHEN position('/' IN substring(object_key FROM length(@prefix::text) + 1)) > 0
         THEN substring(object_key FROM length(@prefix::text) + 1
              FOR position('/' IN substring(object_key FROM length(@prefix::text) + 1)))
         ELSE substring(object_key FROM length(@prefix::text) + 1)
    END)::text AS name,
    (CASE WHEN position('/' IN substring(object_key FROM length(@prefix::text) + 1)) > 0
         THEN true ELSE false
    END)::boolean AS is_dir,
    COUNT(DISTINCT object_key) AS file_count,
    COALESCE(SUM(size_bytes), 0)::bigint AS total_size
FROM object_locations
WHERE object_key LIKE @prefix::text || '%' ESCAPE '\'
  AND length(object_key) > length(@prefix::text)
GROUP BY name, is_dir
ORDER BY is_dir DESC, name ASC;

-- name: ListExpiredObjects :many
SELECT DISTINCT ON (object_key) object_key, backend_name, size_bytes, created_at
FROM object_locations
WHERE object_key LIKE @prefix::text || '%' ESCAPE '\'
  AND created_at < @cutoff
ORDER BY object_key, created_at ASC
LIMIT @max_keys;

-- name: BackendObjectStats :one
SELECT COUNT(*) AS object_count, COALESCE(SUM(size_bytes), 0)::bigint AS total_bytes
FROM object_locations
WHERE backend_name = $1;

-- name: DeleteObjectLocationsByBackend :exec
DELETE FROM object_locations WHERE backend_name = $1;

-- name: ListEncryptedLocations :many
SELECT object_key, backend_name, encryption_key, key_id
FROM object_locations
WHERE encrypted = TRUE AND key_id = $1
ORDER BY object_key, backend_name
LIMIT $2 OFFSET $3;

-- name: UpdateEncryptionKey :exec
UPDATE object_locations
SET encryption_key = $3, key_id = $4
WHERE object_key = $1 AND backend_name = $2;

-- name: ListUnencryptedLocations :many
SELECT object_key, backend_name, size_bytes
FROM object_locations
WHERE encrypted = FALSE
ORDER BY object_key, backend_name
LIMIT $1 OFFSET $2;

-- name: MarkObjectEncrypted :exec
UPDATE object_locations
SET encrypted = TRUE,
    encryption_key = $3,
    key_id = $4,
    plaintext_size = $5,
    size_bytes = $6
WHERE object_key = $1 AND backend_name = $2;

-- name: ListAllEncryptedLocations :many
SELECT object_key, backend_name, size_bytes, encryption_key, key_id, plaintext_size
FROM object_locations
WHERE encrypted = TRUE
ORDER BY object_key, backend_name
LIMIT $1 OFFSET $2;

-- name: MarkObjectDecrypted :exec
UPDATE object_locations
SET encrypted = FALSE,
    encryption_key = NULL,
    key_id = NULL,
    size_bytes = $3,
    plaintext_size = NULL
WHERE object_key = $1 AND backend_name = $2;

-- name: GetRandomHashedObjects :many
-- Return random object locations that have a content hash, for scrubber
-- verification. Uses TABLESAMPLE to avoid a full table sort, then filters
-- and limits. The sample percentage is generous (10%) to ensure enough rows
-- pass the WHERE filter; the LIMIT caps the final result.
SELECT object_key, backend_name, size_bytes, encrypted, encryption_key, key_id, plaintext_size, content_hash, created_at
FROM object_locations TABLESAMPLE BERNOULLI (10)
WHERE content_hash IS NOT NULL
LIMIT $1;

-- name: GetObjectsWithoutHash :many
-- Return object locations that have no content hash, for backfill.
SELECT object_key, backend_name, size_bytes, encrypted, encryption_key, key_id, plaintext_size, content_hash, created_at
FROM object_locations
WHERE content_hash IS NULL
ORDER BY created_at ASC
LIMIT $1 OFFSET $2;

-- name: UpdateContentHash :exec
UPDATE object_locations
SET content_hash = $3
WHERE object_key = $1 AND backend_name = $2;

-- name: GetCopiesForKeysForUpdate :many
-- Returns (object_key, backend_name, size_bytes) for every row matching
-- a key in the supplied list, locked FOR UPDATE so the same transaction
-- can delete the rows and decrement the corresponding backend quotas
-- atomically. Used by the batch-delete path so an N-key request is one
-- transaction instead of N.
SELECT object_key, backend_name, size_bytes
FROM object_locations
WHERE object_key = ANY(@object_keys::text[])
FOR UPDATE;

-- name: DeleteObjectsByKeys :exec
-- Deletes every row whose object_key is in the supplied list. Caller
-- must have already locked the rows via GetCopiesForKeysForUpdate.
DELETE FROM object_locations
WHERE object_key = ANY(@object_keys::text[]);

-- name: GetObjectBackendsForKeys :many
-- Returns (object_key, backend_name) for every object_locations row whose
-- object_key is in the supplied list. Used by the rebalancer planner to
-- determine which backends already hold a copy of each candidate key in a
-- batch, replacing the per-key GetAllObjectLocations N+1 pattern.
SELECT object_key, backend_name
FROM object_locations
WHERE object_key = ANY(@object_keys::text[]);

-- name: ListDirectChildren :many
-- Return per-file detail for non-directory children under a prefix, with pagination.
-- Aggregates every replica into a sorted backend_names array per object_key so
-- the file row reflects all backends a file lives on, not just one.
SELECT
    object_key,
    ARRAY_AGG(backend_name ORDER BY backend_name)::text[] AS backend_names,
    MAX(size_bytes)::bigint AS size_bytes,
    MIN(created_at)::timestamptz AS created_at
FROM object_locations
WHERE object_key LIKE @prefix::text || '%' ESCAPE '\'
  AND position('/' IN substring(object_key FROM length(@prefix::text) + 1)) = 0
  AND length(object_key) > length(@prefix::text)
  AND object_key > @start_after
GROUP BY object_key
ORDER BY object_key
LIMIT @max_keys;
