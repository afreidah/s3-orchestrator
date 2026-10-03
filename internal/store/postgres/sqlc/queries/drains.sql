-- -----------------------------------------------------------------------------
-- Backend Drain Queries
--
-- Author: Alex Freidah
--
-- sqlc-input definitions for backend_drains, the durable record of each
-- backend's drain. Admission reads the table through backend_capacity; these
-- queries are the drain's own lifecycle.
-- -----------------------------------------------------------------------------

-- name: StartDrain :execrows
-- Inserts a draining record, or restarts a failed one. Zero rows affected means
-- the backend is already draining or drained.
INSERT INTO backend_drains (backend_name, state)
VALUES (@backend_name, 'draining')
ON CONFLICT (backend_name) DO UPDATE
SET state = 'draining', objects_moved = 0, last_error = NULL,
    started_at = NOW(), finished_at = NULL
WHERE backend_drains.state = 'failed';

-- name: ListDrains :many
SELECT backend_name, state, objects_moved, last_error, started_at, finished_at
FROM backend_drains
ORDER BY backend_name;

-- name: AddDrainedObjects :exec
UPDATE backend_drains
SET objects_moved = objects_moved + @moved
WHERE backend_name = @backend_name AND state = 'draining';

-- name: MarkDrainFailed :exec
UPDATE backend_drains
SET state = 'failed', last_error = @reason::text, finished_at = NOW()
WHERE backend_name = @backend_name AND state = 'draining';

-- name: CompleteDrain :execrows
-- Marks a drain finished only when nothing it moves is left on the backend: no
-- managed object rows, no intents of writes still uploading, and no multipart
-- uploads. Unmanaged rows are not counted, because a drain leaves them where
-- they are and removing the backend deletes them. One statement reads one
-- snapshot, so a write committing meanwhile is seen either as its intent or as
-- its row, never as neither. Admission already refuses the backend, so the
-- three can only shrink. Zero rows affected means something remains and the
-- drain carries on.
UPDATE backend_drains
SET state = 'drained', finished_at = NOW()
WHERE backend_drains.backend_name = @backend_name
  AND state = 'draining'
  AND NOT EXISTS (SELECT 1 FROM object_locations ol WHERE ol.backend_name = @backend_name AND ol.managed)
  AND NOT EXISTS (SELECT 1 FROM pending_objects po WHERE po.backend_name = @backend_name)
  AND NOT EXISTS (SELECT 1 FROM multipart_uploads mu WHERE mu.backend_name = @backend_name);

-- name: ClearDrain :execrows
DELETE FROM backend_drains WHERE backend_name = @backend_name;
