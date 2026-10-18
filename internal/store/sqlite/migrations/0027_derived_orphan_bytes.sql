-- A backend's orphan bytes are the bytes its cleanup rows still have to
-- delete. Every transaction that removes a copy now queues its cleanup, so
-- backend_capacity sums the queued and dead-lettered rows rather than keeping
-- a counter every overwrite and delete would write, and the column goes.

DROP VIEW IF EXISTS backend_capacity;

ALTER TABLE backend_quotas DROP COLUMN orphan_bytes;

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
