-- -----------------------------------------------------------------------------
-- Pending Object Queries
--
-- Author: Alex Freidah
--
-- sqlc-input definitions for pending_objects - the in-flight PUT intent
-- table that backs the PUT-before-COMMIT write-path pattern. Covers
-- inserting an intent, atomically claiming and resolving it, and the
-- timestamp-aware reaper scan that finds stale rows surviving a failed
-- metadata commit.
-- -----------------------------------------------------------------------------

-- name: InsertPendingObjectIfFits :execrows
-- Claims the bytes and records the intent in one statement, so admission and
-- the durable record of it cannot disagree.
--
-- The headroom is read inside this statement rather than from a snapshot, which
-- is what makes the limit hold across a fleet: every instance's committed
-- bytes, orphans, and writes in progress are rows here, so two instances
-- admitting at once are judged against the same totals. Zero rows affected
-- means the backend had no room or is being drained, and the caller should try
-- the next candidate.
INSERT INTO pending_objects (
    intent_id, object_key, storage_key, backend_name, size_bytes,
    encrypted, encryption_key, key_id, plaintext_size, content_hash,
    compression_algorithm, compression_level, compression_format_version, logical_size,
    etag, content_type, user_metadata, role
)
-- backend_name and size_bytes are cast explicitly because they appear both
-- here and in the headroom test below. A bare parameter in a SELECT list takes
-- no type from the INSERT target the way one in VALUES does, so Postgres would
-- otherwise deduce them from the comparison in the WHERE, come out with
-- integer, and reject the statement for contradicting the bigint column.
SELECT @intent_id, @object_key, @storage_key, @backend_name::text, @size_bytes::bigint,
       @encrypted, @encryption_key, @key_id, @plaintext_size, @content_hash,
       @compression_algorithm, @compression_level, @compression_format_version, @logical_size,
       @etag, @content_type, @user_metadata, @role
FROM backend_capacity c
WHERE c.backend_name = @backend_name::text
  AND c.accepting_writes
  AND (c.available_bytes IS NULL OR c.available_bytes >= @size_bytes::bigint);

-- name: ClearPendingForKey :many
-- Removes the key's intents apart from the ones the caller is committing, and
-- reports what it removed so the caller can clean their bytes off the backends
-- once its own transaction is durable.
--
-- Unconditional even for a backend the caller is writing to: the row left
-- behind would let an upload still in flight commit a copy of the object this
-- write just replaced. Whether those bytes are deleted is the caller's decision,
-- and a different one.
DELETE FROM pending_objects
WHERE object_key = @object_key
  AND intent_id <> ALL(@keep::text[])
RETURNING intent_id, backend_name, storage_key, size_bytes;

-- name: CountPendingOnBackend :one
-- How many intents are live for one key on one backend. A discard asks once
-- its own intent is gone, so a count above zero means other uploads are still
-- landing at the path it was about to delete, and the path is left to them.
SELECT COUNT(*)::bigint
FROM pending_objects
WHERE object_key = @object_key
  AND backend_name = @backend_name;

-- name: DeletePendingObject :exec
DELETE FROM pending_objects WHERE intent_id = $1;

-- name: GetStalePendingObjects :many
-- Return pending intents older than @older_than for reaper resolution.
-- Bounded by @max_keys per call so a backlog cannot starve other queries.
-- Column order follows the table so sqlc projects the row onto the shared
-- pending_objects model rather than minting a query-specific struct; storage_key
-- is last because that is where the migration added it.
SELECT intent_id, object_key, backend_name, size_bytes,
       encrypted, encryption_key, key_id, plaintext_size, content_hash, created_at,
       compression_algorithm, compression_level, compression_format_version, logical_size,
       etag, content_type, user_metadata, role, storage_key
FROM pending_objects
WHERE created_at <= @older_than
ORDER BY created_at ASC
LIMIT @max_keys;

-- name: CountPendingObjects :one
SELECT COUNT(*)::bigint FROM pending_objects;

-- name: DeletePendingObjectsByBackend :exec
-- Used during backend remove/drain finalization so abandoned intents do not
-- outlive their backend's row in backend_quotas (FK cascade safety).
DELETE FROM pending_objects WHERE backend_name = $1;

-- name: LockPendingForUpdate :one
-- Returns the pending row under FOR UPDATE so two concurrent reapers cannot
-- both attempt to promote the same intent. pgx.ErrNoRows means another
-- instance already resolved this intent (deleted the row); the caller
-- treats that as a benign no-op.
-- Column order follows the table so sqlc projects the row onto the shared
-- pending_objects model rather than minting a query-specific struct; storage_key
-- is last because that is where the migration added it.
SELECT intent_id, object_key, backend_name, size_bytes,
       encrypted, encryption_key, key_id, plaintext_size, content_hash, created_at,
       compression_algorithm, compression_level, compression_format_version, logical_size,
       etag, content_type, user_metadata, role, storage_key
FROM pending_objects
WHERE intent_id = $1
FOR UPDATE;
