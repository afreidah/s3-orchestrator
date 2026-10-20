-- -----------------------------------------------------------------------------
-- Multipart Completion Mark
--
-- Author: Alex Freidah
--
-- A completion stamps completion_heartbeat_at before it reads the parts and
-- renews it while it runs. A part upload or abort refuses an upload whose mark
-- is fresh, so neither can delete bytes the assembly is still streaming. A
-- mark not renewed within the grace period belongs to a completion that died
-- and may be taken over. NULL means no completion is running.
-- -----------------------------------------------------------------------------

-- +goose Up
ALTER TABLE multipart_uploads ADD COLUMN completion_heartbeat_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE multipart_uploads DROP COLUMN completion_heartbeat_at;
