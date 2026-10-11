-- Admission refuses a backend with a backend_drains row in any state, so the
-- table records whether a backend takes new writes, not only its drain. Renamed
-- to backend_states so states other than a drain can live in it.
-- backend_capacity is dropped and recreated around the rename rather than
-- relying on SQLite to rewrite the view's reference.

DROP VIEW IF EXISTS backend_capacity;

ALTER TABLE backend_drains RENAME TO backend_states;

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
       st.backend_name IS NULL AS accepting_writes
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
