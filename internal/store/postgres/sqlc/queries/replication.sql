-- -----------------------------------------------------------------------------
-- Replication Queries
--
-- Author: Alex Freidah
--
-- sqlc-input definitions backing the replicator and over-replication
-- cleaner: under-replicated and over-replicated scans, the conditional
-- replica insert that returns the source row's size_bytes, and the
-- excess-copy removal. The "excluding" variant of the under-replicated
-- scan lets workers skip backends that are draining or circuit-broken.
-- -----------------------------------------------------------------------------

-- name: GetUnderReplicatedObjects :many
WITH under_replicated AS (
    SELECT object_key
    FROM object_locations
    WHERE managed
    GROUP BY object_key
    HAVING COUNT(*) < @factor::bigint
    LIMIT @max_keys
)
SELECT ol.object_key, ol.backend_name, ol.size_bytes, ol.encrypted, ol.encryption_key, ol.key_id, ol.plaintext_size, ol.content_hash, ol.compression_algorithm, ol.compression_level, ol.compression_format_version, ol.logical_size, ol.created_at
FROM object_locations ol
JOIN under_replicated ur ON ol.object_key = ur.object_key
ORDER BY ol.object_key ASC, ol.created_at ASC;

-- name: GetUnderReplicatedObjectsExcluding :many
WITH under_replicated AS (
    SELECT object_key
    FROM object_locations
    WHERE backend_name != ALL(@excluded::text[]) AND managed
    GROUP BY object_key
    HAVING COUNT(*) < @factor::bigint
    LIMIT @max_keys
)
SELECT ol.object_key, ol.backend_name, ol.size_bytes, ol.encrypted, ol.encryption_key, ol.key_id, ol.plaintext_size, ol.content_hash, ol.compression_algorithm, ol.compression_level, ol.compression_format_version, ol.logical_size, ol.created_at
FROM object_locations ol
JOIN under_replicated ur ON ol.object_key = ur.object_key
ORDER BY ol.object_key ASC, ol.created_at ASC;

-- name: GetOverReplicatedObjects :many
WITH over_replicated AS (
    SELECT object_key
    FROM object_locations
    WHERE managed
    GROUP BY object_key
    HAVING COUNT(*) > @factor::bigint
    LIMIT @max_keys
)
SELECT ol.object_key, ol.backend_name, ol.size_bytes, ol.encrypted, ol.encryption_key, ol.key_id, ol.plaintext_size, ol.content_hash, ol.compression_algorithm, ol.compression_level, ol.compression_format_version, ol.logical_size, ol.created_at
FROM object_locations ol
JOIN over_replicated orep ON ol.object_key = orep.object_key
ORDER BY ol.object_key ASC, ol.created_at ASC;

-- name: CountOverReplicatedObjects :one
SELECT COUNT(*)::bigint AS count
FROM (
    SELECT object_key
    FROM object_locations
    WHERE managed
    GROUP BY object_key
    HAVING COUNT(*) > @factor::bigint
) over_replicated;

-- name: InsertReplicaConditional :one
-- Returns the size_bytes that was actually inserted into object_locations
-- (read from the source row in the same statement). Caller uses this size
-- for IncrementBackendQuota so object_locations.size_bytes and
-- backend_quotas.bytes_used always agree, even if the in-memory copy size
-- the caller observed before InsertReplicaConditional differs from the
-- source row's current size_bytes (e.g. a concurrent overwrite landed
-- between GetUnderReplicatedObjects and the conditional insert).
-- ON CONFLICT or missing source returns no rows; the caller treats that
-- as inserted=false.
INSERT INTO object_locations (object_key, backend_name, size_bytes, encrypted, encryption_key, key_id, plaintext_size, content_hash, compression_algorithm, compression_level, compression_format_version, logical_size, created_at)
SELECT $1, $2, ol.size_bytes, ol.encrypted, ol.encryption_key, ol.key_id, ol.plaintext_size, ol.content_hash, ol.compression_algorithm, ol.compression_level, ol.compression_format_version, ol.logical_size, NOW()
FROM object_locations ol
WHERE ol.object_key = $1 AND ol.backend_name = $3
ON CONFLICT (object_key, backend_name) DO NOTHING
RETURNING size_bytes;
