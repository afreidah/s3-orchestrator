-- -----------------------------------------------------------------------------
-- Backend State Queries
--
-- Author: Alex Freidah
--
-- sqlc-input definitions for backend_states: each backend's state flags and
-- its most recent drain's progress. Admission reads the flags through
-- backend_capacity. Every flag value is a parameter, so which bit means what is
-- defined once, in Go.
-- -----------------------------------------------------------------------------

-- name: ListBackendStates :many
SELECT backend_name, flags, objects_moved, last_error, started_at, finished_at
FROM backend_states
ORDER BY backend_name;

-- name: SetBackendFlag :execrows
-- Sets one flag, creating the row when the backend has none. Zero rows
-- affected means the flag was already set.
INSERT INTO backend_states (backend_name, flags)
VALUES (@backend_name, @flag::bigint)
ON CONFLICT (backend_name) DO UPDATE
SET flags = backend_states.flags | @flag::bigint
WHERE (backend_states.flags & @flag::bigint) = 0;

-- name: ClearBackendFlags :execrows
-- Clears the given flags. Zero rows affected means none of them was set.
UPDATE backend_states
SET flags = flags & ~(@clear_flags::bigint)
WHERE backend_name = @backend_name AND (flags & @clear_flags::bigint) <> 0;

-- name: StartDrain :execrows
-- Starts a drain, replacing a failed one, and resets its progress. Other flags,
-- such as a cordon, are kept. Zero rows affected means the backend is already
-- draining or drained.
INSERT INTO backend_states (backend_name, flags)
VALUES (@backend_name, @draining::bigint)
ON CONFLICT (backend_name) DO UPDATE
SET flags = (backend_states.flags & ~(@drain_flags::bigint)) | @draining::bigint,
    objects_moved = 0, last_error = NULL, started_at = NOW(), finished_at = NULL
WHERE (backend_states.flags & @busy::bigint) = 0;

-- name: AddDrainedObjects :exec
UPDATE backend_states
SET objects_moved = objects_moved + @moved
WHERE backend_name = @backend_name AND (flags & @draining::bigint) <> 0;

-- name: MarkDrainFailed :exec
UPDATE backend_states
SET flags = (flags & ~(@drain_flags::bigint)) | @failed::bigint,
    last_error = @reason::text, finished_at = NOW()
WHERE backend_name = @backend_name AND (flags & @draining::bigint) <> 0;

-- name: CompleteDrain :execrows
-- Marks a drain finished only when nothing it moves is left on the backend: no
-- managed object rows, no intents of writes still uploading, and no multipart
-- uploads. Unmanaged rows are not counted, because a drain leaves them where
-- they are and removing the backend deletes them. One statement reads one
-- snapshot, so a write committing meanwhile is seen either as its intent or as
-- its row, never as neither. Admission already refuses the backend, so the
-- three can only shrink. Zero rows affected means something remains and the
-- drain carries on.
UPDATE backend_states
SET flags = (flags & ~(@drain_flags::bigint)) | @drained::bigint, finished_at = NOW()
WHERE backend_states.backend_name = @backend_name
  AND (flags & @draining::bigint) <> 0
  AND NOT EXISTS (SELECT 1 FROM object_locations ol WHERE ol.backend_name = @backend_name AND ol.managed)
  AND NOT EXISTS (SELECT 1 FROM pending_objects po WHERE po.backend_name = @backend_name)
  AND NOT EXISTS (SELECT 1 FROM multipart_uploads mu WHERE mu.backend_name = @backend_name);

-- name: ClearBackendState :execrows
-- Deletes the backend's row, which removing the backend does.
DELETE FROM backend_states WHERE backend_name = @backend_name;
