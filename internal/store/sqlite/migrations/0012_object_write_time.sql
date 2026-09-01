-- created_at is the object's write time and reaches clients as Last-Modified.
-- Replication used to stamp its own value on each new copy instead of carrying
-- the source's, so copies of one key disagree by however long replication took.
-- A read answers from whichever copy served it, which made an unmodified object
-- report a different Last-Modified after a failover.
--
-- Writes are fixed at the source, so only rows written before this migration
-- need repair. The oldest stamp is the correct one: the copy the client's own
-- write created is the one that was stamped at the write, and every later value
-- belongs to a replica.
--
-- This moves timestamps backwards, never forwards, so an object can only look
-- older afterwards. Lifecycle expiry reads the same column, so an object whose
-- replicas carried a later stamp now ages from its real write time.

UPDATE object_locations
SET created_at = (
    SELECT MIN(sibling.created_at)
    FROM object_locations sibling
    WHERE sibling.object_key = object_locations.object_key
)
WHERE created_at <> (
    SELECT MIN(sibling.created_at)
    FROM object_locations sibling
    WHERE sibling.object_key = object_locations.object_key
);
