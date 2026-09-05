-- backend_quotas.bytes_used was a single row per backend, so every write that
-- charged a backend took the same row lock. Splitting the counter across stripe
-- rows removes that contention on Postgres; SQLite serializes writers at the
-- database level and gains nothing from it, but both engines carry the same
-- schema so the quota arithmetic in store/core stays one implementation.
--
-- A writer picks its stripe from the object key, so every charge and credit for
-- one key lands on the same row. Stripes are signed rather than clamped at
-- zero: the backfill puts all pre-existing bytes on stripe zero, so deleting an
-- object recorded before this migration credits whichever stripe its key hashes
-- to and drives that row negative while the sum stays correct. Clamping belongs
-- on the total, never on a stripe.

CREATE TABLE IF NOT EXISTS backend_quota_stripes (
    backend_name TEXT    NOT NULL REFERENCES backend_quotas(backend_name) ON DELETE CASCADE,
    stripe_id    INTEGER NOT NULL,
    bytes_used   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (backend_name, stripe_id)
);

INSERT INTO backend_quota_stripes (backend_name, stripe_id, bytes_used)
SELECT backend_name, 0, bytes_used
FROM backend_quotas
WHERE true
ON CONFLICT (backend_name, stripe_id) DO NOTHING;

ALTER TABLE backend_quotas DROP COLUMN bytes_used;
