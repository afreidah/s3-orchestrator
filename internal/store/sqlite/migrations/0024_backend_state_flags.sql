-- A backend's state was one value, so a cordon and a drain could not be held at
-- once. It is now a set of flags in one integer: cordoned (1), draining (2),
-- drained (4) and drain failed (8). Bits are only ever added, never renumbered.
-- A row with no flags set is a backend in normal service.
--
-- backend_capacity carries each backend's flags in place of accepting_writes.
-- Admission tests them against an accept mask the store passes in, so which
-- flags a write tolerates is defined once, in Go.

DROP VIEW IF EXISTS backend_capacity;

ALTER TABLE backend_states ADD COLUMN flags INTEGER NOT NULL DEFAULT 0;

UPDATE backend_states SET flags = CASE state
    WHEN 'draining' THEN 2
    WHEN 'drained'  THEN 4
    WHEN 'failed'   THEN 8
    ELSE 0
END;

ALTER TABLE backend_states DROP COLUMN state;

CREATE VIEW IF NOT EXISTS backend_capacity AS
SELECT q.backend_name,
       q.bytes_limit,
       MAX(0, COALESCE(s.bytes_used, 0)) AS bytes_used,
       q.orphan_bytes,
       COALESCE(m.inflight, 0) + COALESCE(p.inflight, 0) AS inflight_bytes,
       CASE WHEN q.bytes_limit = 0 THEN NULL
            ELSE q.bytes_limit
                 - MAX(0, COALESCE(s.bytes_used, 0))
                 - q.orphan_bytes
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
