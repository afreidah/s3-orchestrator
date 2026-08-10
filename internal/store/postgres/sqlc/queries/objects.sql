-- -----------------------------------------------------------------------------
-- Object Location Queries
--
-- Author: Alex Freidah
--
-- sqlc-input definitions for object_locations - the canonical (object_key,
-- backend_name) ledger of which backends hold which objects. Includes the
-- transactional advisory lock primitive (LockObjectKeyForWrite) used by the
-- write path, the FOR UPDATE locks used by the per-key transactional helpers
-- in core/, and the listing queries the manager and dashboard consume.
-- -----------------------------------------------------------------------------

-- name: LockObjectKeyForWrite :exec
SELECT pg_advisory_xact_lock(hashtext($1));

-- name: GetExistingCopiesForUpdate :many
SELECT backend_name, size_bytes, created_at, encrypted,
       (encryption_key IS NOT NULL AND length(encryption_key) > 0) AS has_dek
FROM object_locations
WHERE object_key = $1
FOR UPDATE;

-- name: DeleteObjectCopies :exec
DELETE FROM object_locations
WHERE object_key = $1;

-- name: InsertObjectLocation :exec
INSERT INTO object_locations (object_key, backend_name, size_bytes, encrypted, encryption_key, key_id, plaintext_size, content_hash, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW());

-- ListObjectsByBackend backs the rebalance, placement and drain candidate
-- scans, so it returns managed rows only. Objects outside every configured
-- bucket prefix are tracked for accounting but are not the orchestrator's to
-- move.
-- name: ListObjectsByBackend :many
SELECT object_key, backend_name, size_bytes, created_at
FROM object_locations
WHERE backend_name = $1 AND managed
ORDER BY size_bytes ASC
LIMIT $2;

-- ListObjectsByBackendKeyAsc returns rows for a backend in ascending object_key
-- order, starting strictly after the supplied cursor. Used by ReconcileBackend
-- to drive a bounded-memory sorted-merge join against an S3 ListObjects walk.
-- Pass '' as the cursor on the first call.
--
-- COLLATE "C" is required: the merge join compares keys in byte order (Go string
-- comparison) against S3 ListObjectsV2, which is UTF-8 byte ordered. Without it,
-- a locale-collated object_key column orders the cursor differently, the merge
-- mis-pairs keys, and reconcile oscillates (false imports/removes that never
-- converge). The cursor predicate and ORDER BY must use the same collation.
-- name: ListObjectsByBackendKeyAsc :many
SELECT object_key, backend_name, size_bytes, created_at
FROM object_locations
WHERE backend_name = $1 AND object_key COLLATE "C" > $2
ORDER BY object_key COLLATE "C" ASC
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
INSERT INTO object_locations (object_key, backend_name, size_bytes, encrypted, encryption_key, key_id, plaintext_size, content_hash, managed, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW())
ON CONFLICT (object_key, backend_name) DO NOTHING
RETURNING true AS inserted;

-- name: GetDirectoryStats :many
-- Aggregate count and size for immediate children of a directory prefix.
-- Directories (containing a '/') and files are distinguished by is_dir.
-- file_count counts distinct object_keys (logical files); total_size sums
-- every replica row in object_locations so directory totals reflect real
-- physical storage consumption, matching the Storage Summary semantics.
-- @prefix is LIKE-escaped for the WHERE match; @name_start is the 1-based
-- character position where the child name begins (len(unescaped prefix) + 1),
-- so escaped wildcards (e.g. '\_') don't shift the substring cut point.
SELECT
    (CASE WHEN position('/' IN substr(object_key, @name_start::int)) > 0
         THEN substr(object_key, @name_start::int,
              position('/' IN substr(object_key, @name_start::int)))
         ELSE substr(object_key, @name_start::int)
    END)::text AS name,
    (CASE WHEN position('/' IN substr(object_key, @name_start::int)) > 0
         THEN true ELSE false
    END)::boolean AS is_dir,
    COUNT(DISTINCT object_key) AS file_count,
    COALESCE(SUM(size_bytes), 0)::bigint AS total_size
FROM object_locations
WHERE object_key LIKE @prefix::text || '%' ESCAPE '\'
  AND length(object_key) >= @name_start::int
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

-- name: GetLeastRecentlyScrubbedObjects :many
-- Return the copies least recently touched, by verification or by writing.
--
-- Falling back to created_at is what keeps the sweep alive on a busy fleet: a
-- copy written moments ago sorts to the back rather than jumping the queue, so
-- a write rate above the scrub rate cannot starve older data. It also puts the
-- effort where rot actually accumulates, since churn is deleted long before it
-- degrades while the copies that persist for months are the ones at risk.
SELECT object_key, backend_name, size_bytes, encrypted, encryption_key, key_id, plaintext_size, content_hash, created_at
FROM object_locations
WHERE content_hash IS NOT NULL AND managed
ORDER BY COALESCE(last_scrubbed_at, created_at) ASC, object_key ASC
LIMIT $1;

-- name: MarkObjectScrubbed :exec
-- Stamp a copy the scrubber just examined. Applied to every attempted copy,
-- not only the ones that verified: a copy that cannot be read would otherwise
-- stay at the head of the queue and starve the rest of the sweep.
UPDATE object_locations
SET last_scrubbed_at = NOW()
WHERE object_key = $1 AND backend_name = $2;

-- name: OldestUnverifiedAge :one
-- Age in seconds of the least recently verified copy, which is the figure that
-- says whether integrity checking is keeping up. Never-verified copies count
-- as infinitely old, so they dominate until the first full sweep completes.
SELECT
    COALESCE(EXTRACT(EPOCH FROM (NOW() - MIN(last_scrubbed_at))), 0)::bigint AS age_seconds,
    COUNT(*) FILTER (WHERE last_scrubbed_at IS NULL)::bigint AS never_verified
FROM object_locations
WHERE content_hash IS NOT NULL AND managed;

-- name: GetObjectsWithoutHash :many
-- Return object locations that have no content hash, for backfill. Hashing
-- reads the whole body, so unmanaged rows are left alone rather than spending
-- egress on data the orchestrator does not manage.
SELECT object_key, backend_name, size_bytes, encrypted, encryption_key, key_id, plaintext_size, content_hash, created_at
FROM object_locations
WHERE content_hash IS NULL AND managed
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

-- name: ListObjectsDelimited :many
WITH RECURSIVE walk(k) AS (
    (SELECT object_key
       FROM object_locations
      WHERE object_key LIKE @escprefix::text || '%' ESCAPE '\'
        AND object_key COLLATE "C" > @start_after::text
      ORDER BY object_key COLLATE "C"
      LIMIT 1)
    UNION ALL
    SELECT (
        SELECT object_key
          FROM object_locations
         WHERE object_key LIKE @escprefix::text || '%' ESCAPE '\'
           AND object_key COLLATE "C" > CASE
            WHEN position(@delim::text IN substr(walk.k, length(@prefix::text) + 1)) > 0 THEN
                substr(walk.k, 1, length(@prefix::text) + position(@delim::text IN substr(walk.k, length(@prefix::text) + 1)) + length(@delim::text) - 2)
                || chr(ascii(substr(walk.k, length(@prefix::text) + position(@delim::text IN substr(walk.k, length(@prefix::text) + 1)) + length(@delim::text) - 1, 1)) + 1)
            ELSE walk.k
           END
         ORDER BY object_key COLLATE "C"
         LIMIT 1
    )
    FROM walk WHERE walk.k IS NOT NULL
)
-- Every projected column is forced non-null (empty string / 0 / epoch) because
-- the built-in sqlc analyzer cannot infer nullability for computed and
-- LATERAL-joined columns; the Go side uses is_prefix to pick the meaningful
-- fields per row, so the placeholder values for the other branch are ignored.
SELECT
    w.k::text AS object_key,
    (position(@delim::text IN substr(w.k, length(@prefix::text) + 1)) > 0)::boolean AS is_prefix,
    (CASE WHEN position(@delim::text IN substr(w.k, length(@prefix::text) + 1)) > 0
        THEN substr(w.k, 1, length(@prefix::text) + position(@delim::text IN substr(w.k, length(@prefix::text) + 1)) + length(@delim::text) - 1)
        ELSE '' END)::text AS common_prefix,
    (CASE WHEN position(@delim::text IN substr(w.k, length(@prefix::text) + 1)) > 0 THEN
            substr(w.k, 1, length(@prefix::text) + position(@delim::text IN substr(w.k, length(@prefix::text) + 1)) + length(@delim::text) - 2)
            || chr(ascii(substr(w.k, length(@prefix::text) + position(@delim::text IN substr(w.k, length(@prefix::text) + 1)) + length(@delim::text) - 1, 1)) + 1)
        ELSE w.k END)::text AS skip_bound,
    COALESCE(leaf.backend_name, '')::text AS backend_name,
    COALESCE(leaf.size_bytes, 0)::bigint AS size_bytes,
    COALESCE(leaf.created_at, to_timestamp(0)) AS created_at
FROM walk w
LEFT JOIN LATERAL (
    SELECT backend_name, size_bytes, created_at
      FROM object_locations o2
     WHERE o2.object_key = w.k
       AND position(@delim::text IN substr(w.k, length(@prefix::text) + 1)) = 0
     ORDER BY created_at ASC
     LIMIT 1
) leaf ON true
WHERE w.k IS NOT NULL
ORDER BY w.k COLLATE "C"
LIMIT @lim::int;
