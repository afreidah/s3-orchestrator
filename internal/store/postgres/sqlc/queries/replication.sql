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
-- A key's copies are the rows it holds plus the companion intents still
-- uploading one, because a write that places its own copies commits them a
-- moment apart and the intent is the statement that the copy is on its way.
-- Counting only the rows makes every such write look under-replicated for that
-- moment, and a scan landing inside it reads the object back to make a copy the
-- write is already placing.
WITH inflight AS (
    SELECT object_key, COUNT(*) AS copies
    FROM pending_objects
    WHERE role = 'companion'
    GROUP BY object_key
),
under_replicated AS (
    SELECT ol.object_key
    FROM object_locations ol
    LEFT JOIN inflight i ON i.object_key = ol.object_key
    WHERE ol.managed
    GROUP BY ol.object_key, i.copies
    HAVING COUNT(*) + COALESCE(i.copies, 0) < @factor::bigint
    LIMIT @max_keys
)
SELECT ol.object_key, ol.backend_name, ol.size_bytes, ol.encrypted, ol.encryption_key, ol.key_id, ol.plaintext_size, ol.content_hash, ol.compression_algorithm, ol.compression_level, ol.compression_format_version, ol.logical_size, ol.created_at
FROM object_locations ol
JOIN under_replicated ur ON ol.object_key = ur.object_key
ORDER BY ol.object_key ASC, ol.created_at ASC;

-- name: GetUnderReplicatedObjectsExcluding :many
-- Counts a key's in-flight copies the same way GetUnderReplicatedObjects does.
-- An intent on an excluded backend still counts, because excluding a backend
-- says the worker will not place a copy there, not that a copy already going
-- there is absent.
WITH inflight AS (
    SELECT object_key, COUNT(*) AS copies
    FROM pending_objects
    WHERE role = 'companion'
    GROUP BY object_key
),
under_replicated AS (
    SELECT ol.object_key
    FROM object_locations ol
    LEFT JOIN inflight i ON i.object_key = ol.object_key
    WHERE ol.backend_name != ALL(@excluded::text[]) AND ol.managed
    GROUP BY ol.object_key, i.copies
    HAVING COUNT(*) + COALESCE(i.copies, 0) < @factor::bigint
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
-- (read from the source row in the same statement), which is what the caller
-- charges the backend so the row and the counter always agree even if a
-- concurrent overwrite changed the source between the caller's scan and this
-- insert. ON CONFLICT, a missing source, or a target without room returns no
-- rows; the caller treats that as inserted=false and tries the next candidate.
--
-- The target's headroom is tested here rather than by the caller beforehand.
-- A replica is admitted the same way a PUT is - against live rows, inside the
-- statement that claims the space - so two instances replicating at once are
-- judged against the same totals rather than each against its own view.
--
-- created_at is carried from the source rather than stamped NOW(): it is the
-- object's write time, and it reaches clients as Last-Modified. Stamping it
-- per copy makes an unmodified object report a different time depending on
-- which replica answered, and moves that time again whenever the oldest copy
-- is rebalanced away.
INSERT INTO object_locations (object_key, backend_name, size_bytes, encrypted, encryption_key, key_id, plaintext_size, content_hash, compression_algorithm, compression_level, compression_format_version, logical_size, etag, content_type, user_metadata, created_at)
-- Cast for the reason the pending claim casts: these parameters appear both in
-- this SELECT list, where a bare parameter takes no type from the INSERT
-- target, and in the predicates below.
SELECT @object_key::text, @target_backend::text, ol.size_bytes, ol.encrypted, ol.encryption_key, ol.key_id, ol.plaintext_size, ol.content_hash, ol.compression_algorithm, ol.compression_level, ol.compression_format_version, ol.logical_size, ol.etag, ol.content_type, ol.user_metadata, ol.created_at
FROM object_locations ol
JOIN backend_quotas q ON q.backend_name = @target_backend::text
LEFT JOIN (
    SELECT backend_name, SUM(bytes_used) AS bytes_used
    FROM backend_quota_stripes GROUP BY backend_name
) s ON s.backend_name = q.backend_name
LEFT JOIN (
    SELECT mu.backend_name, SUM(mp.size_bytes) AS inflight
    FROM multipart_uploads mu
    JOIN multipart_parts mp ON mp.upload_id = mu.upload_id
    GROUP BY mu.backend_name
) m ON m.backend_name = q.backend_name
LEFT JOIN (
    SELECT backend_name, SUM(size_bytes) AS inflight
    FROM pending_objects GROUP BY backend_name
) p ON p.backend_name = q.backend_name
WHERE ol.object_key = @object_key::text AND ol.backend_name = @source_backend
  AND (q.bytes_limit = 0
       OR q.bytes_limit
          - GREATEST(0, COALESCE(s.bytes_used, 0))::bigint
          - q.orphan_bytes
          - COALESCE(m.inflight, 0)
          - COALESCE(p.inflight, 0) >= ol.size_bytes)
ON CONFLICT (object_key, backend_name) DO NOTHING
RETURNING size_bytes;
