-- -------------------------------------------------------------------------------
-- Align created_at Across Copies of an Object
--
-- Author: Alex Freidah
--
-- created_at is the object's write time and reaches clients as Last-Modified.
-- Replication used to stamp NOW() on each new copy instead of carrying the
-- source's value, so copies of one key disagree by however long replication
-- took. A read answers from whichever copy served it, which made an unmodified
-- object report a different Last-Modified after a failover, and left
-- If-Modified-Since and If-Range comparing against a value that moved.
--
-- Writes are fixed at the source, so only rows written before this migration
-- need repair. The oldest stamp is the correct one: the copy the client's own
-- write created is the one that was stamped at the write, and every later
-- value belongs to a replica.
--
-- MIN over the key rather than a per-row update, so the whole set converges on
-- one value in a single pass. Rows already holding the minimum are excluded so
-- the write touches only what actually disagrees.
--
-- This moves timestamps backwards, never forwards, so an object can only look
-- older afterwards. Lifecycle expiry reads the same column: an object whose
-- replicas were carrying a later stamp now ages from its real write time, which
-- is the intended behaviour but can bring an object closer to expiry than the
-- pre-migration value suggested.
-- -------------------------------------------------------------------------------

-- +goose Up

UPDATE object_locations ol
SET created_at = m.min_created_at
FROM (
    SELECT object_key, MIN(created_at) AS min_created_at
    FROM object_locations
    GROUP BY object_key
) m
WHERE ol.object_key = m.object_key
  AND ol.created_at IS DISTINCT FROM m.min_created_at;

-- +goose Down

-- Irreversible: the per-copy stamps this replaced are not recoverable, and the
-- aligned value is the correct one regardless of engine version.
SELECT 1;
