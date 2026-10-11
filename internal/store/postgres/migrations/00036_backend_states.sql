-- -----------------------------------------------------------------------------
-- Backend States
--
-- Author: Alex Freidah
--
-- Admission refuses a backend with a backend_drains row in any state, so the
-- table records whether a backend takes new writes, not only its drain. Renamed
-- to backend_states so states other than a drain can live in it.
-- backend_capacity follows the rename, since Postgres views reference the
-- table rather than its name.
-- -----------------------------------------------------------------------------

-- +goose Up
ALTER TABLE backend_drains RENAME TO backend_states;
ALTER TABLE backend_states RENAME CONSTRAINT backend_drains_state_check TO backend_states_state_check;
ALTER INDEX backend_drains_pkey RENAME TO backend_states_pkey;

-- +goose Down
ALTER INDEX backend_states_pkey RENAME TO backend_drains_pkey;
ALTER TABLE backend_states RENAME CONSTRAINT backend_states_state_check TO backend_drains_state_check;
ALTER TABLE backend_states RENAME TO backend_drains;
