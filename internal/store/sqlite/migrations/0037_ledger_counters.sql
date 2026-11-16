-- Each stripe row splits further by the stored form and managed flag of the
-- copies it counts, and counts their copies, object bytes and never-scrubbed
-- copies beside the stored bytes it already held. The transaction that changes
-- a copy charges these, so the per-backend ledger figures are read from the
-- rows instead of a grouped pass over object_locations. The primary key grows,
-- so the table is rebuilt, with the counters filled from that pass onto stripe
-- zero, and backend_capacity is recreated over it.

DROP VIEW IF EXISTS backend_capacity;

CREATE TABLE backend_quota_stripes_counted (
    backend_name   TEXT    NOT NULL REFERENCES backend_quotas(backend_name) ON DELETE CASCADE,
    stripe_id      INTEGER NOT NULL,
    form           INTEGER NOT NULL DEFAULT 0,
    managed        INTEGER NOT NULL DEFAULT 1,
    copies         INTEGER NOT NULL DEFAULT 0,
    bytes_used     INTEGER NOT NULL DEFAULT 0,
    object_bytes   INTEGER NOT NULL DEFAULT 0,
    never_scrubbed INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (backend_name, stripe_id, form, managed)
);

INSERT INTO backend_quota_stripes_counted (backend_name, stripe_id, form, managed, copies, bytes_used, object_bytes, never_scrubbed)
SELECT l.backend_name, 0, l.form, l.managed, COUNT(*), SUM(l.size_bytes), SUM(l.object_size),
    SUM(CASE WHEN l.last_scrubbed_at IS NULL THEN 1 ELSE 0 END)
FROM object_locations l
JOIN backend_quotas q ON q.backend_name = l.backend_name
GROUP BY l.backend_name, l.form, l.managed;

DROP TABLE backend_quota_stripes;

ALTER TABLE backend_quota_stripes_counted RENAME TO backend_quota_stripes;

CREATE VIEW IF NOT EXISTS backend_capacity AS
SELECT q.backend_name,
       q.bytes_limit,
       MAX(0, COALESCE(s.bytes_used, 0)) AS bytes_used,
       COALESCE(o.orphan_bytes, 0) AS orphan_bytes,
       COALESCE(m.inflight, 0) + COALESCE(p.inflight, 0) AS inflight_bytes,
       CASE WHEN q.bytes_limit = 0 THEN NULL
            ELSE q.bytes_limit
                 - MAX(0, COALESCE(s.bytes_used, 0))
                 - COALESCE(o.orphan_bytes, 0)
                 - COALESCE(m.inflight, 0)
                 - COALESCE(p.inflight, 0)
       END AS available_bytes,
       COALESCE(st.flags, 0) AS flags
FROM backend_quotas q
LEFT JOIN (
    SELECT backend_name, SUM(bytes_used) AS bytes_used
    FROM backend_quota_stripes
    GROUP BY backend_name
) s ON s.backend_name = q.backend_name
LEFT JOIN (
    SELECT backend_name, SUM(size_bytes) AS orphan_bytes
    FROM (
        SELECT backend_name, size_bytes FROM cleanup_queue
        UNION ALL
        SELECT backend_name, size_bytes FROM cleanup_dlq
    )
    GROUP BY backend_name
) o ON o.backend_name = q.backend_name
LEFT JOIN (
    SELECT mu.backend_name, SUM(mp.size_bytes) AS inflight
    FROM multipart_uploads mu
    JOIN multipart_parts mp ON mp.upload_id = mu.upload_id
    GROUP BY mu.backend_name
) m ON m.backend_name = q.backend_name
LEFT JOIN (
    SELECT backend_name, SUM(size_bytes) AS inflight
    FROM pending_objects
    GROUP BY backend_name
) p ON p.backend_name = q.backend_name
LEFT JOIN backend_states st ON st.backend_name = q.backend_name;
