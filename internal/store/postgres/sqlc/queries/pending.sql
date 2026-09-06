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
-- means the backend had no room and the caller should try the next candidate.
--
-- bytes_limit = 0 is unlimited, matching every other reader of the column.
INSERT INTO pending_objects (
    intent_id, object_key, backend_name, size_bytes,
    encrypted, encryption_key, key_id, plaintext_size, content_hash,
    compression_algorithm, compression_level, compression_format_version, logical_size,
    etag, content_type, user_metadata, role
)
-- backend_name and size_bytes are cast explicitly because they appear both
-- here and in the headroom test below. A bare parameter in a SELECT list takes
-- no type from the INSERT target the way one in VALUES does, so Postgres would
-- otherwise deduce them from the arithmetic in the WHERE, come out with
-- integer, and reject the statement for contradicting the bigint column.
SELECT @intent_id, @object_key, @backend_name::text, @size_bytes::bigint,
       @encrypted, @encryption_key, @key_id, @plaintext_size, @content_hash,
       @compression_algorithm, @compression_level, @compression_format_version, @logical_size,
       @etag, @content_type, @user_metadata, @role
FROM backend_quotas q
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
WHERE q.backend_name = @backend_name::text
  AND (q.bytes_limit = 0
       OR q.bytes_limit
          - GREATEST(0, COALESCE(s.bytes_used, 0))::bigint
          - q.orphan_bytes
          - COALESCE(m.inflight, 0)
          - COALESCE(p.inflight, 0) >= @size_bytes::bigint);

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
RETURNING intent_id, backend_name, size_bytes;

-- name: DeletePendingObject :exec
DELETE FROM pending_objects WHERE intent_id = $1;

-- name: GetStalePendingObjects :many
-- Return pending intents older than @older_than for reaper resolution.
-- Bounded by @max_keys per call so a backlog cannot starve other queries.
SELECT intent_id, object_key, backend_name, size_bytes,
       encrypted, encryption_key, key_id, plaintext_size, content_hash, created_at,
       compression_algorithm, compression_level, compression_format_version, logical_size,
       etag, content_type, user_metadata, role
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
SELECT intent_id, object_key, backend_name, size_bytes,
       encrypted, encryption_key, key_id, plaintext_size, content_hash, created_at,
       compression_algorithm, compression_level, compression_format_version, logical_size,
       etag, content_type, user_metadata, role
FROM pending_objects
WHERE intent_id = $1
FOR UPDATE;
