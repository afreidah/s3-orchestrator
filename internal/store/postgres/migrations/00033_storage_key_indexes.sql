-- -------------------------------------------------------------------------------
-- Indexes for the Per-Write Storage Key
--
-- Author: Alex Freidah
--
-- Reconcile's sorted-merge join now walks the ledger by storage_key, because
-- that is what a backend listing returns: the path the bytes occupy, which is
-- no longer the object's key. The cursor predicate and the ORDER BY carry
-- COLLATE "C" for the same reason 00012 added it for object_key - S3
-- ListObjectsV2 is UTF-8 byte ordered and the merge compares Go strings, so a
-- locale-collated walk mis-pairs keys and the pass oscillates.
--
-- The unique index states the invariant the whole change exists to create: one
-- backend holds one object at one path. It is satisfiable on an existing
-- deployment by construction - every backfilled row has storage_key =
-- object_key and (object_key, backend_name) is already the primary key - and
-- from here on a new path carries a fresh intent id, so a violation would mean
-- two writes minted the same 128-bit id.
-- -------------------------------------------------------------------------------

-- +goose Up
-- +goose NO TRANSACTION

-- Backs ListObjectsByBackendKeyAsc: WHERE backend_name = $1
-- AND storage_key COLLATE "C" > $2 ORDER BY storage_key COLLATE "C" ASC.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_object_locations_backend_storage_key_collate_c
    ON object_locations (backend_name, storage_key COLLATE "C");

CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_object_locations_backend_storage_key_unique
    ON object_locations (backend_name, storage_key);

DROP INDEX CONCURRENTLY IF EXISTS idx_object_locations_backend_key_collate_c;

-- +goose Down
-- +goose NO TRANSACTION

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_object_locations_backend_key_collate_c
    ON object_locations (backend_name, object_key COLLATE "C");

DROP INDEX CONCURRENTLY IF EXISTS idx_object_locations_backend_storage_key_unique;
DROP INDEX CONCURRENTLY IF EXISTS idx_object_locations_backend_storage_key_collate_c;
