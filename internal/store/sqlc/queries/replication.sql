-- name: GetUnderReplicatedObjects :many
WITH under_replicated AS (
    SELECT object_key
    FROM object_locations
    GROUP BY object_key
    HAVING COUNT(*) < @factor::bigint
    LIMIT @max_keys
)
SELECT ol.object_key, ol.backend_name, ol.size_bytes, ol.encrypted, ol.encryption_key, ol.key_id, ol.plaintext_size, ol.content_hash, ol.created_at
FROM object_locations ol
JOIN under_replicated ur ON ol.object_key = ur.object_key
ORDER BY ol.object_key, ol.created_at ASC;

-- name: GetUnderReplicatedObjectsExcluding :many
WITH under_replicated AS (
    SELECT object_key
    FROM object_locations
    WHERE backend_name != ALL(@excluded::text[])
    GROUP BY object_key
    HAVING COUNT(*) < @factor::bigint
    LIMIT @max_keys
)
SELECT ol.object_key, ol.backend_name, ol.size_bytes, ol.encrypted, ol.encryption_key, ol.key_id, ol.plaintext_size, ol.content_hash, ol.created_at
FROM object_locations ol
JOIN under_replicated ur ON ol.object_key = ur.object_key
ORDER BY ol.object_key, ol.created_at ASC;

-- name: GetOverReplicatedObjects :many
WITH over_replicated AS (
    SELECT object_key
    FROM object_locations
    GROUP BY object_key
    HAVING COUNT(*) > @factor::bigint
    LIMIT @max_keys
)
SELECT ol.object_key, ol.backend_name, ol.size_bytes, ol.encrypted, ol.encryption_key, ol.key_id, ol.plaintext_size, ol.content_hash, ol.created_at
FROM object_locations ol
JOIN over_replicated orep ON ol.object_key = orep.object_key
ORDER BY ol.object_key, ol.created_at ASC;

-- name: CountOverReplicatedObjects :one
SELECT COUNT(*)::bigint AS count
FROM (
    SELECT object_key
    FROM object_locations
    GROUP BY object_key
    HAVING COUNT(*) > @factor::bigint
) over_replicated;

-- name: GetObjectCopiesForUpdate :many
SELECT object_key, backend_name, size_bytes, encrypted, encryption_key, key_id, plaintext_size, content_hash, created_at
FROM object_locations
WHERE object_key = $1
FOR UPDATE;

-- name: InsertReplicaConditional :one
INSERT INTO object_locations (object_key, backend_name, size_bytes, encrypted, encryption_key, key_id, plaintext_size, content_hash, created_at)
SELECT $1, $2, ol.size_bytes, ol.encrypted, ol.encryption_key, ol.key_id, ol.plaintext_size, ol.content_hash, NOW()
FROM object_locations ol
WHERE ol.object_key = $1 AND ol.backend_name = $3
ON CONFLICT (object_key, backend_name) DO NOTHING
RETURNING true AS inserted;
