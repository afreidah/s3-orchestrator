-- -----------------------------------------------------------------------------
-- Bucket Storage Settings
--
-- Author: Alex Freidah
--
-- How a bucket's new objects are stored. stored_form is the layers they carry,
-- as the bits of the stored-form flags object_locations.form uses: NULL takes
-- the deployment's, which every existing bucket does, and zero is no layer at
-- all. replication_factor is how many copies each object is kept at, zero for
-- the deployment's.
-- -----------------------------------------------------------------------------

-- +goose Up

ALTER TABLE buckets ADD COLUMN stored_form BIGINT;
ALTER TABLE buckets ADD COLUMN replication_factor INTEGER NOT NULL DEFAULT 0;

-- +goose Down

ALTER TABLE buckets DROP COLUMN IF EXISTS replication_factor;
ALTER TABLE buckets DROP COLUMN IF EXISTS stored_form;
