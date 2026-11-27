-- -----------------------------------------------------------------------------
-- Replication Queries
--
-- Author: Alex Freidah
--
-- sqlc-input definitions backing the replication queue: the writes that queue
-- the keys a transaction left off the factor, the rebuild that queues every one
-- the ledger holds, and the drain's read, settle, retry and count.
--
-- A key's count is its managed copies plus the intents still uploading one,
-- because a write that places its own copies commits them a moment apart and
-- the intent is the statement that the copy is on its way. Every live intent
-- counts, whatever its role: the first copy to land may be a companion while the
-- primary is still in flight. One left by a write that never finished holds the
-- key off only until the reaper clears it.
--
-- Copies on an excluded backend do not count toward a short key, but an intent
-- there still does: excluding a backend says no copy will be placed there, not
-- that a copy already going there is absent. A key past the factor counts every
-- recorded copy. An empty list excludes nothing.
--
-- Every queue write makes the key due now with fresh attempts and bumps its
-- version, so a drain that read an older version cannot settle it away.
-- -----------------------------------------------------------------------------

-- name: QueueReplication :exec
-- Queues keys the transaction knows the side of, in the order given.
INSERT INTO replication_queue (object_key, direction)
SELECT unnest(@object_keys::text[]), unnest(@directions::smallint[])
ON CONFLICT (object_key) DO UPDATE
SET direction = EXCLUDED.direction, next_retry = NOW(), attempts = 0, version = replication_queue.version + 1;

-- name: QueueOffFactor :exec
-- Counts each key and queues those off factor. A key with no copy and no intent
-- is gone and needs nothing.
INSERT INTO replication_queue (object_key, direction)
SELECT counted.object_key, (CASE WHEN counted.live < @factor::bigint THEN -1 ELSE 1 END)::smallint
FROM (
    SELECT keys.object_key,
        (SELECT COUNT(*) FROM object_locations ol WHERE ol.object_key = keys.object_key AND ol.managed)
      + (SELECT COUNT(*) FROM pending_objects p WHERE p.object_key = keys.object_key) AS live
    FROM unnest(@object_keys::text[]) AS keys(object_key)
) counted
WHERE counted.live > 0 AND counted.live <> @factor::bigint
ON CONFLICT (object_key) DO UPDATE
SET direction = EXCLUDED.direction, next_retry = NOW(), attempts = 0, version = replication_queue.version + 1;

-- name: RebuildReplicationQueue :execrows
-- One grouped pass over the ledger that queues every key off its factor, for
-- factors or an excluded set the queue was not kept against. A key is kept at
-- the factor its bucket sets in bucket_names and bucket_factors, and otherwise
-- at default_factor.
WITH inflight AS (
    SELECT object_key, COUNT(*) AS copies
    FROM pending_objects
    GROUP BY object_key
),
overrides AS (
    SELECT unnest(@bucket_names::text[]) AS bucket, unnest(@bucket_factors::bigint[]) AS factor
),
counted AS (
    SELECT ol.object_key,
           COUNT(*) FILTER (WHERE ol.backend_name != ALL(@excluded::text[])) + COALESCE(i.copies, 0) AS live,
           COUNT(*) AS held
    FROM object_locations ol
    LEFT JOIN inflight i ON i.object_key = ol.object_key
    WHERE ol.managed
    GROUP BY ol.object_key, i.copies
),
factored AS (
    SELECT c.object_key, c.live, c.held, COALESCE(o.factor, @default_factor::bigint) AS factor
    FROM counted c
    LEFT JOIN overrides o ON o.bucket = split_part(c.object_key, '/', 1)
)
INSERT INTO replication_queue (object_key, direction)
SELECT object_key, (CASE WHEN live < factor THEN -1 ELSE 1 END)::smallint
FROM factored
WHERE live < factor OR held > factor
ON CONFLICT (object_key) DO UPDATE
SET direction = EXCLUDED.direction, next_retry = NOW(), attempts = 0, version = replication_queue.version + 1;

-- name: ListReplicationQueue :many
-- Reads a side's next due keys with each key's count. The replicator's lock
-- keeps one drain at a time, so the read holds nothing.
SELECT rq.object_key, rq.direction, rq.attempts, rq.version,
    ((SELECT COUNT(*) FROM object_locations ol
      WHERE ol.object_key = rq.object_key AND ol.managed AND ol.backend_name != ALL(@excluded::text[]))
   + (SELECT COUNT(*) FROM pending_objects p WHERE p.object_key = rq.object_key))::bigint AS live,
    (SELECT COUNT(*) FROM object_locations ol
     WHERE ol.object_key = rq.object_key AND ol.managed)::bigint AS held
FROM replication_queue rq
WHERE rq.direction = @direction::smallint AND rq.next_retry <= NOW()
ORDER BY rq.direction, rq.next_retry
LIMIT @row_limit;

-- name: SettleReplicationQueue :exec
-- Drops a key the drain brought back to the factor, unless a write queued it
-- again since the drain read it.
DELETE FROM replication_queue
WHERE object_key = @object_key AND version = @version;

-- name: RetryReplicationQueue :exec
-- Holds a key the drain left off factor until next_retry, unless a write queued
-- it again since the drain read it.
UPDATE replication_queue
SET attempts = attempts + 1, next_retry = @next_retry::timestamptz
WHERE object_key = @object_key AND version = @version;

-- name: CountReplicationQueue :one
SELECT COUNT(*) FILTER (WHERE direction < 0)::bigint AS under_replicated,
       COUNT(*) FILTER (WHERE direction > 0)::bigint AS over_replicated
FROM replication_queue;
