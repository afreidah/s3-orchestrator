-- +goose Up
-- -----------------------------------------------------------------------------
-- Striped Quota Counters
--
-- Author: Alex Freidah
--
-- backend_quotas.bytes_used was a single row per backend, so every write that
-- charged a backend took the same row lock and concurrent writes to one backend
-- serialized behind each other for the length of their transactions. Splitting
-- the counter across stripe rows removes that: row locks are per row, so
-- writers landing on different stripes never wait on one another, and the
-- backend's total is the sum across its stripes.
--
-- A writer picks its stripe from the object key, so every charge and credit for
-- one key lands on the same row. Stripes are signed rather than clamped at
-- zero: the backfill puts all pre-existing bytes on stripe zero, so deleting an
-- object recorded before this migration credits whichever stripe its key hashes
-- to and drives that row negative while the sum stays correct. Clamping belongs
-- on the total, never on a stripe.
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS backend_quota_stripes (
    backend_name TEXT     NOT NULL REFERENCES backend_quotas(backend_name) ON DELETE CASCADE,
    stripe_id    SMALLINT NOT NULL,
    bytes_used   BIGINT   NOT NULL DEFAULT 0,
    PRIMARY KEY (backend_name, stripe_id)
);

-- Carry the existing counter onto stripe zero so the sum reads the same value
-- the moment the new queries take over.
INSERT INTO backend_quota_stripes (backend_name, stripe_id, bytes_used)
SELECT backend_name, 0, bytes_used
FROM backend_quotas
ON CONFLICT (backend_name, stripe_id) DO NOTHING;

ALTER TABLE backend_quotas DROP COLUMN bytes_used;

-- +goose Down
ALTER TABLE backend_quotas ADD COLUMN bytes_used BIGINT NOT NULL DEFAULT 0;

UPDATE backend_quotas q
SET bytes_used = GREATEST(0, COALESCE(s.total, 0))
FROM (
    SELECT backend_name, SUM(bytes_used) AS total
    FROM backend_quota_stripes
    GROUP BY backend_name
) s
WHERE s.backend_name = q.backend_name;

DROP TABLE IF EXISTS backend_quota_stripes;
