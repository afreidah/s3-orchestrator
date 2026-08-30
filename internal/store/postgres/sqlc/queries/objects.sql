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
INSERT INTO object_locations (object_key, backend_name, size_bytes, encrypted, encryption_key, key_id, plaintext_size, content_hash, compression_algorithm, compression_level, compression_format_version, logical_size, etag, content_type, user_metadata, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, NOW());

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
-- Every column describing the stored bytes, because the caller moving this row
-- to another backend rebuilds the destination from what this returns. A column
-- missing here is one the moved copy silently stops claiming, which for the
-- compression columns means a still-encoded object recorded as verbatim.
--
-- The probe columns come along because a verbatim move does not change the
-- bytes, so what the encoder measured about them still holds. Dropping them
-- would have the next pass download and encode the copy again to learn what
-- this row already knows.
SELECT size_bytes, encrypted, encryption_key, key_id, plaintext_size, content_hash,
       compression_algorithm, compression_level, compression_format_version, logical_size,
       compression_probe_size, compression_probe_level, etag, content_type, user_metadata
FROM object_locations
WHERE object_key = $1 AND backend_name = $2
FOR UPDATE;

-- name: DeleteObjectFromBackend :exec
DELETE FROM object_locations
WHERE object_key = $1 AND backend_name = $2;

-- name: ListObjectsByPrefix :many
-- COLLATE "C" is required: S3 ListObjectsV2 returns keys in UTF-8 byte order,
-- and object_key is plain TEXT so it would otherwise sort under the database's
-- LC_COLLATE. The cursor predicate carries the same collation as the ORDER BY -
-- splitting them would page a byte-ordered scan with a locale-ordered cursor and
-- skip or repeat keys. DISTINCT ON must carry it too, or Postgres rejects the
-- query for not matching the leading ORDER BY expression.
SELECT DISTINCT ON (object_key COLLATE "C") object_key, backend_name, size_bytes, etag, created_at
FROM object_locations
WHERE object_key LIKE @prefix::text || '%' ESCAPE '\'
  AND object_key COLLATE "C" > @start_after
ORDER BY object_key COLLATE "C", created_at ASC
LIMIT @max_keys;

-- name: GetAllObjectLocations :many
SELECT object_key, backend_name, size_bytes, encrypted, encryption_key, key_id, plaintext_size, content_hash, compression_algorithm, compression_level, compression_format_version, logical_size, etag, content_type, user_metadata, created_at, last_scrubbed_at
FROM object_locations
WHERE object_key = $1
ORDER BY created_at ASC;

-- name: InsertObjectLocationIfNotExists :one
INSERT INTO object_locations (object_key, backend_name, size_bytes, encrypted, encryption_key, key_id, plaintext_size, content_hash, compression_algorithm, compression_level, compression_format_version, logical_size, etag, content_type, user_metadata, managed, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, NOW())
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
-- Collated for the same reason as ListObjectsByPrefix. The expiry worker does
-- not depend on the order, but a batch that differs by engine is one more thing
-- an operator has to hold in their head when a run is reproduced elsewhere.
--
-- The tag filter is a correlated subquery rather than a join: a join against
-- object_tags multiplies the row per matching tag, and this query's DISTINCT ON
-- and its matching ORDER BY are what reduce an object's replicas to one row.
-- Counting in a subquery leaves both untouched.
--
-- Requiring the count to equal tag_count is what makes several tags an AND.
-- The primary key allows one row per (object_key, tag_key), so a count equal to
-- the number of pairs asked for means every one of them matched.
SELECT DISTINCT ON (ol.object_key COLLATE "C") ol.object_key, ol.backend_name, ol.size_bytes, ol.created_at
FROM object_locations ol
WHERE ol.object_key LIKE @prefix::text || '%' ESCAPE '\'
  AND ol.created_at < @cutoff
  AND (
    @tag_count::int = 0
    OR (
      SELECT COUNT(*)
      FROM object_tags t
      JOIN jsonb_each_text(@tags::jsonb) AS f(k, v)
        ON t.tag_key = f.k AND t.tag_value = f.v
      WHERE t.object_key = ol.object_key
    ) = @tag_count::int
  )
ORDER BY ol.object_key COLLATE "C", ol.created_at ASC
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

-- name: CountUnencryptedLocations :one
-- Copies still stored as plaintext. Uses the same predicate as
-- ListUnencryptedLocations, so the figure is exactly what encrypt-existing
-- would process rather than a differently-scoped count that happens to be near
-- it.
SELECT count(*) FROM object_locations WHERE encrypted = FALSE;

-- name: CompressionStats :many
-- What compression is worth, per backend. Only encoded copies are counted:
-- including the verbatim ones would report a ratio no encoder produced. The
-- saving is logical - stored, left to the caller so it cannot disagree with the
-- two figures it comes from.
SELECT backend_name,
       count(*) AS objects,
       COALESCE(SUM(logical_size), 0)::bigint AS logical_bytes,
       COALESCE(SUM(size_bytes), 0)::bigint AS stored_bytes
FROM object_locations
WHERE compression_algorithm IS NOT NULL
GROUP BY backend_name;

-- name: ListUnencryptedLocations :many
-- Paged by cursor rather than offset. Encrypting a copy takes it out of this
-- predicate, so the set shrinks as encrypt-existing walks it and an offset
-- would step over the rows that moved up.
SELECT object_key, backend_name, size_bytes
FROM object_locations
WHERE encrypted = FALSE
  AND (object_key, backend_name) > (sqlc.arg(after_key)::text, sqlc.arg(after_backend)::text)
ORDER BY object_key, backend_name
LIMIT sqlc.arg(row_limit);

-- name: MarkObjectEncrypted :exec
UPDATE object_locations
SET encrypted = TRUE,
    encryption_key = $3,
    key_id = $4,
    plaintext_size = $5,
    size_bytes = $6
WHERE object_key = $1 AND backend_name = $2;

-- name: ListAllEncryptedLocations :many
-- Cursor-paged for the same reason as ListUnencryptedLocations: decrypting a
-- copy removes it from this set mid-walk.
SELECT object_key, backend_name, size_bytes, encryption_key, key_id, plaintext_size
FROM object_locations
WHERE encrypted = TRUE
  AND (object_key, backend_name) > (sqlc.arg(after_key)::text, sqlc.arg(after_backend)::text)
ORDER BY object_key, backend_name
LIMIT sqlc.arg(row_limit);

-- name: ListUncompressedLocations :many
-- Copies whose stored bytes carry no encoding, which is what compress-existing
-- rewrites. The encryption columns come along because compression sits inside
-- encryption: an encrypted copy is decrypted, encoded, and re-encrypted under
-- the key it already had.
--
-- Paged by cursor, not offset: an encoded copy leaves this predicate, so the
-- set shrinks under the pass walking it.
--
-- Copies already measured as not worth encoding are excluded here rather than
-- downloaded and encoded again to reach the same verdict, which on an
-- incompressible fleet is the pass's largest wasted expense. The stored
-- measurement is judged against the current settings, so it excludes a copy
-- only while it would still be declined: lowering min_ratio returns those
-- copies to the pass without reading any of them. A probe taken at a different
-- level says nothing about this one and is ignored, since the levels are names
-- from an ordered set rather than numbers.
--
-- The divisor is NULLIF'd because a zero-length copy cannot shrink: the
-- comparison goes NULL and the row is excluded, matching WorthStoring, which
-- declines a logical size of zero outright.
--
-- The size floor is applied here for the same reason, one the row can answer:
-- a copy below it is never a candidate, so listing it only to decline it costs
-- a page slot on every pass forever.
SELECT object_key, backend_name, size_bytes, encrypted, encryption_key, key_id,
       plaintext_size, compression_algorithm, compression_level,
       compression_format_version, logical_size
FROM object_locations
WHERE compression_algorithm IS NULL
  AND (CASE WHEN encrypted THEN plaintext_size ELSE size_bytes END) >= sqlc.arg(min_size)::bigint
  AND (compression_probe_size IS NULL
       OR compression_probe_level IS DISTINCT FROM sqlc.arg(probe_level)::text
       OR compression_probe_size::float8
          / NULLIF(CASE WHEN encrypted THEN plaintext_size ELSE size_bytes END, 0)::float8
          <= sqlc.arg(min_ratio)::float8)
  AND (object_key, backend_name) > (sqlc.arg(after_key)::text, sqlc.arg(after_backend)::text)
ORDER BY object_key, backend_name
LIMIT sqlc.arg(row_limit);

-- name: ListCompressedLocations :many
-- The complement of ListUncompressedLocations, which is what
-- decompress-existing rewrites. Cursor-paged for the same reason, and the case
-- that makes it matter most: every object this pass succeeds on leaves the
-- predicate, so an offset walk would skip whole pages and stop early.
SELECT object_key, backend_name, size_bytes, encrypted, encryption_key, key_id,
       plaintext_size, compression_algorithm, compression_level,
       compression_format_version, logical_size
FROM object_locations
WHERE compression_algorithm IS NOT NULL
  AND (object_key, backend_name) > (sqlc.arg(after_key)::text, sqlc.arg(after_backend)::text)
ORDER BY object_key, backend_name
LIMIT sqlc.arg(row_limit);

-- name: MarkObjectCompressed :exec
-- Records how a rewritten copy is now stored. A NULL algorithm is the
-- decompress direction, which also clears the columns that only describe an
-- encoding. The envelope columns are rewritten too: re-encrypting an object
-- mints a new base nonce and wrapped key, so leaving the old ones would
-- describe bytes nothing can decrypt.
UPDATE object_locations
SET compression_algorithm = $3,
    compression_level = $4,
    compression_format_version = $5,
    logical_size = $6,
    size_bytes = $7,
    plaintext_size = $8,
    encryption_key = $9,
    key_id = $10
WHERE object_key = $1 AND backend_name = $2;

-- name: RecordCompressionProbe :exec
-- Records what the encoder produced for a copy it declined to store compressed,
-- so the next pass reaches the same verdict from the row instead of downloading
-- and encoding the object again.
--
-- Only the min_ratio decline writes here. A min_size decline is answered from
-- the row at no cost, a copy declined by usage limits never reached the encoder,
-- and a failure measured nothing.
UPDATE object_locations
SET compression_probe_size = $3,
    compression_probe_level = $4
WHERE object_key = $1 AND backend_name = $2;

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
--
-- backend_names restricts the batch to copies the scrubber can afford to read.
-- Filtering here rather than after selection is what keeps a sweep useful when
-- a backend is over its usage limit: a copy the scrubber would decline never
-- occupies a slot, so it is neither stamped as examined nor left at the head of
-- the queue to be re-selected every cycle.
SELECT object_key, backend_name, size_bytes, encrypted, encryption_key, key_id, plaintext_size, content_hash, compression_algorithm, compression_level, compression_format_version, logical_size, created_at, last_scrubbed_at
FROM object_locations
WHERE content_hash IS NOT NULL AND managed
  AND backend_name = ANY(@backend_names::text[])
ORDER BY COALESCE(last_scrubbed_at, created_at) ASC, object_key ASC
LIMIT @row_limit;

-- name: CountScrubCandidatesOnBackends :one
-- Copies eligible for scrubbing that live on the named backends. Used to report
-- how much of the queue a cycle declined to read, which a sampled count of the
-- batch cannot show.
SELECT count(*)
FROM object_locations
WHERE content_hash IS NOT NULL AND managed
  AND backend_name = ANY(@backend_names::text[]);

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
SELECT object_key, backend_name, size_bytes, encrypted, encryption_key, key_id, plaintext_size, content_hash, compression_algorithm, compression_level, compression_format_version, logical_size, created_at
FROM object_locations
WHERE content_hash IS NULL AND managed
ORDER BY created_at ASC
LIMIT $1 OFFSET $2;

-- name: UpdateContentHash :exec
UPDATE object_locations
SET content_hash = $3
WHERE object_key = $1 AND backend_name = $2;

-- RecordObjectIdentity fills in what a read had to ask a backend for, so the
-- next one does not. Every copy of the key is written, not just the one that
-- answered: a per-copy value is what lets a failover change the ETag under a
-- conditional request, which is the divergence this column exists to end.
--
-- Only NULL columns are filled. A recorded identity is what the write computed
-- over the client's own bytes, and a backend's answer must never overwrite it.
-- name: RecordObjectIdentity :exec
UPDATE object_locations
SET etag          = COALESCE(etag, sqlc.narg('etag')),
    content_type  = COALESCE(content_type, sqlc.narg('content_type')),
    user_metadata = COALESCE(user_metadata, sqlc.narg('user_metadata'))
WHERE object_key = $1;

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
    COALESCE(leaf.etag, '')::text AS etag,
    COALESCE(leaf.created_at, to_timestamp(0)) AS created_at
FROM walk w
LEFT JOIN LATERAL (
    SELECT backend_name, size_bytes, etag, created_at
      FROM object_locations o2
     WHERE o2.object_key = w.k
       AND position(@delim::text IN substr(w.k, length(@prefix::text) + 1)) = 0
     ORDER BY created_at ASC
     LIMIT 1
) leaf ON true
WHERE w.k IS NOT NULL
ORDER BY w.k COLLATE "C"
LIMIT @lim::int;
