-- Renames the resource kind a control-plane grant names after the component it
-- describes. "instance" read as one process of a deployment running several,
-- while the grant has always covered every process: they all authorize against
-- these same rows.
--
-- The reader accepts both spellings, so a deployment that has not run this yet
-- still authorizes its existing grants. This migration is what stops the old
-- spelling accumulating in new rows.

UPDATE grants SET resource_kind = 'orchestrator' WHERE resource_kind = 'instance';
