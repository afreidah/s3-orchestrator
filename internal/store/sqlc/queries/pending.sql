-- name: InsertPendingObject :exec
INSERT INTO pending_objects (
    intent_id, object_key, backend_name, size_bytes,
    encrypted, encryption_key, key_id, plaintext_size, content_hash
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: DeletePendingObject :exec
DELETE FROM pending_objects WHERE intent_id = $1;

-- name: GetStalePendingObjects :many
-- Return pending intents older than @older_than for reaper resolution.
-- Bounded by @max_keys per call so a backlog cannot starve other queries.
SELECT intent_id, object_key, backend_name, size_bytes,
       encrypted, encryption_key, key_id, plaintext_size, content_hash, created_at
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
       encrypted, encryption_key, key_id, plaintext_size, content_hash, created_at
FROM pending_objects
WHERE intent_id = $1
FOR UPDATE;
