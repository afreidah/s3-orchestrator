-- -----------------------------------------------------------------------------
-- Replication Queries
--
-- Author: Alex Freidah
--
-- sqlc-input definitions backing the replicator and over-replication
-- cleaner: the under- and over-replicated scans and the backlog count.
-- -----------------------------------------------------------------------------

-- name: GetUnderReplicatedObjects :many
-- A key's copies are the rows it holds plus the intents still uploading one,
-- because a write that places its own copies commits them a moment apart and
-- the intent is the statement that the copy is on its way. Counting only the
-- rows makes every such write look under-replicated for that moment, and a
-- scan landing inside it reads the object back to make a copy the write is
-- already placing.
--
-- Every live intent counts, whatever its role. The role tells the reaper what
-- to do with an abandoned intent; it does not say which copy records the
-- object. That is the first to land, and when it is a companion the write's
-- primary intent is the one still in flight: a count of companions alone read
-- such a key as one copy short, and the worker copied it to the very backend
-- the primary was landing on. An intent a later write to the same key adds
-- counts too, harmlessly, since that write replaces the object; one left by a
-- write that never finished holds replication off only until the reaper
-- clears it.
--
-- Copies on an excluded backend do not count, but an intent there still does:
-- excluding a backend says the worker will not place a copy there, not that a
-- copy already going there is absent. An empty list excludes nothing.
WITH inflight AS (
    SELECT object_key, COUNT(*) AS copies
    FROM pending_objects
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
SELECT ol.object_key, ol.backend_name, ol.storage_key, ol.size_bytes, ol.encrypted, ol.encryption_key, ol.key_id, ol.plaintext_size, ol.content_hash, ol.compression_algorithm, ol.compression_level, ol.compression_format_version, ol.logical_size, ol.created_at
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
SELECT ol.object_key, ol.backend_name, ol.storage_key, ol.size_bytes, ol.encrypted, ol.encryption_key, ol.key_id, ol.plaintext_size, ol.content_hash, ol.compression_algorithm, ol.compression_level, ol.compression_format_version, ol.logical_size, ol.created_at
FROM object_locations ol
JOIN over_replicated orep ON ol.object_key = orep.object_key
ORDER BY ol.object_key ASC, ol.created_at ASC;

-- name: CountReplicationBacklog :one
-- One grouped pass for both directions. Under applies GetUnderReplicatedObjects'
-- in-flight rule; over counts recorded copies only, as GetOverReplicatedObjects
-- does.
WITH inflight AS (
    SELECT object_key, COUNT(*) AS copies
    FROM pending_objects
    GROUP BY object_key
),
copies AS (
    SELECT COUNT(*) AS held, COALESCE(i.copies, 0) AS inflight
    FROM object_locations ol
    LEFT JOIN inflight i ON i.object_key = ol.object_key
    WHERE ol.managed
    GROUP BY ol.object_key, i.copies
)
SELECT COUNT(*) FILTER (WHERE held + inflight < @factor::bigint)::bigint AS under_replicated,
       COUNT(*) FILTER (WHERE held > @factor::bigint)::bigint AS over_replicated
FROM copies;
