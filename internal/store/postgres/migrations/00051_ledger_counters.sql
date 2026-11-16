-- -----------------------------------------------------------------------------
-- Ledger Counters
--
-- Author: Alex Freidah
--
-- Each stripe row splits further by the stored form and managed flag of the
-- copies it counts, and counts their copies, object bytes and never-scrubbed
-- copies beside the stored bytes it already held. The transaction that changes
-- a copy charges these, so the per-backend ledger figures are read from the
-- rows instead of a grouped pass over object_locations. The counters are rebuilt
-- from that pass onto stripe zero, the way the stripes were first filled.
-- -----------------------------------------------------------------------------

-- +goose Up

ALTER TABLE backend_quota_stripes
    ADD COLUMN form BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN managed BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN copies BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN object_bytes BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN never_scrubbed BIGINT NOT NULL DEFAULT 0,
    DROP CONSTRAINT backend_quota_stripes_pkey,
    ADD PRIMARY KEY (backend_name, stripe_id, form, managed);

DELETE FROM backend_quota_stripes;

INSERT INTO backend_quota_stripes (backend_name, stripe_id, form, managed, copies, bytes_used, object_bytes, never_scrubbed)
SELECT l.backend_name, 0, l.form, l.managed, COUNT(*), SUM(l.size_bytes), SUM(l.object_size),
    COUNT(*) FILTER (WHERE l.last_scrubbed_at IS NULL)
FROM object_locations l
JOIN backend_quotas q ON q.backend_name = l.backend_name
GROUP BY l.backend_name, l.form, l.managed;

-- +goose Down

CREATE TEMP TABLE backend_quota_stripe_bytes AS
SELECT backend_name, stripe_id, SUM(bytes_used) AS bytes_used
FROM backend_quota_stripes
GROUP BY backend_name, stripe_id;

DELETE FROM backend_quota_stripes;

ALTER TABLE backend_quota_stripes
    DROP CONSTRAINT backend_quota_stripes_pkey,
    DROP COLUMN form,
    DROP COLUMN managed,
    DROP COLUMN copies,
    DROP COLUMN object_bytes,
    DROP COLUMN never_scrubbed,
    ADD PRIMARY KEY (backend_name, stripe_id);

INSERT INTO backend_quota_stripes (backend_name, stripe_id, bytes_used)
SELECT backend_name, stripe_id, bytes_used FROM backend_quota_stripe_bytes;

DROP TABLE backend_quota_stripe_bytes;
