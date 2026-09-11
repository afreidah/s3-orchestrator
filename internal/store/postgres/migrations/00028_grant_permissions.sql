-- -----------------------------------------------------------------------------
-- Grant Permissions
--
-- Author: Alex Freidah
--
-- Gives a grant the access it carries, so a bucket can be granted read-only.
-- Until now a grant authorized everything within the bucket it named.
--
-- Stored as a comma-separated list rather than an integer so a row read in psql
-- says what it allows, and rather than a column per permission so a fourth one
-- is not another migration. Nothing filters on it in SQL: grants are read whole
-- on every registry assembly and compiled into a bit set there.
--
-- Empty means every permission. Existing grants are left empty rather than
-- backfilled, because narrowing them on upgrade would refuse clients that were
-- working before it.
-- -----------------------------------------------------------------------------

-- +goose Up
ALTER TABLE grants ADD COLUMN IF NOT EXISTS permissions TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE grants DROP COLUMN IF EXISTS permissions;
