-- name: GetExistingCopiesForUpdate :many
SELECT backend_name, size_bytes
FROM object_locations
WHERE object_key = $1
FOR UPDATE;

-- name: DeleteObjectCopies :exec
DELETE FROM object_locations
WHERE object_key = $1;

-- name: InsertObjectLocation :exec
INSERT INTO object_locations (object_key, backend_name, size_bytes, created_at)
VALUES ($1, $2, $3, NOW());

-- name: ListObjectsByBackend :many
SELECT object_key, backend_name, size_bytes, created_at
FROM object_locations
WHERE backend_name = $1
ORDER BY size_bytes ASC
LIMIT $2;

-- name: CheckObjectExistsOnBackend :one
SELECT EXISTS(
    SELECT 1 FROM object_locations
    WHERE object_key = $1 AND backend_name = $2
) AS exists;

-- name: LockObjectOnBackend :one
SELECT size_bytes
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
SELECT object_key, backend_name, size_bytes, created_at
FROM object_locations
WHERE object_key = $1
ORDER BY created_at ASC;

-- name: InsertObjectLocationIfNotExists :one
INSERT INTO object_locations (object_key, backend_name, size_bytes, created_at)
VALUES ($1, $2, $3, NOW())
ON CONFLICT (object_key, backend_name) DO NOTHING
RETURNING true AS inserted;

-- name: GetDirectoryStats :many
-- Aggregate count and size for immediate children of a directory prefix.
-- Directories (containing a '/') and files are distinguished by is_dir.
SELECT
    (CASE WHEN position('/' IN substring(object_key FROM length(@prefix::text) + 1)) > 0
         THEN substring(object_key FROM length(@prefix::text) + 1
              FOR position('/' IN substring(object_key FROM length(@prefix::text) + 1)))
         ELSE substring(object_key FROM length(@prefix::text) + 1)
    END)::text AS name,
    (CASE WHEN position('/' IN substring(object_key FROM length(@prefix::text) + 1)) > 0
         THEN true ELSE false
    END)::boolean AS is_dir,
    COUNT(DISTINCT object_key)  AS file_count,
    COALESCE(SUM(size_bytes), 0)::bigint AS total_size
FROM object_locations
WHERE object_key LIKE @prefix::text || '%' ESCAPE '\'
  AND length(object_key) > length(@prefix::text)
GROUP BY name, is_dir
ORDER BY is_dir DESC, name ASC;

-- name: ListDirectChildren :many
-- Return per-file detail for non-directory children under a prefix, with pagination.
SELECT DISTINCT ON (object_key) object_key, backend_name, size_bytes, created_at
FROM object_locations
WHERE object_key LIKE @prefix::text || '%' ESCAPE '\'
  AND position('/' IN substring(object_key FROM length(@prefix::text) + 1)) = 0
  AND length(object_key) > length(@prefix::text)
  AND object_key > @start_after
ORDER BY object_key, created_at ASC
LIMIT @max_keys;
