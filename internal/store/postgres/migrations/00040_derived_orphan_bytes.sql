-- -----------------------------------------------------------------------------
-- Derived Orphan Bytes
--
-- Author: Alex Freidah
--
-- A backend's orphan bytes are the bytes its cleanup rows still have to
-- delete. Every transaction that removes a copy now queues its cleanup, so a
-- counter on backend_quotas would be written by every overwrite and delete.
-- backend_capacity sums the queued and dead-lettered rows instead, the way it
-- already sums intents and multipart parts, and the counter column goes.
-- -----------------------------------------------------------------------------

-- +goose Up
DROP VIEW backend_capacity;

ALTER TABLE backend_quotas DROP COLUMN orphan_bytes;

CREATE VIEW backend_capacity AS
SELECT q.backend_name,
       q.bytes_limit,
       GREATEST(0, COALESCE(s.bytes_used, 0))::bigint AS bytes_used,
       COALESCE(o.orphan_bytes, 0)::bigint AS orphan_bytes,
       (COALESCE(m.inflight, 0) + COALESCE(p.inflight, 0))::bigint AS inflight_bytes,
       CASE WHEN q.bytes_limit = 0 THEN NULL
            ELSE (q.bytes_limit
                  - GREATEST(0, COALESCE(s.bytes_used, 0))
                  - COALESCE(o.orphan_bytes, 0)
                  - COALESCE(m.inflight, 0)
                  - COALESCE(p.inflight, 0))::bigint
       END AS available_bytes,
       COALESCE(st.flags, 0)::bigint AS flags
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
    ) queued
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

-- +goose Down
DROP VIEW backend_capacity;

ALTER TABLE backend_quotas ADD COLUMN orphan_bytes BIGINT NOT NULL DEFAULT 0;

UPDATE backend_quotas q
SET orphan_bytes = queued.total
FROM (
    SELECT backend_name, SUM(size_bytes) AS total
    FROM (
        SELECT backend_name, size_bytes FROM cleanup_queue
        UNION ALL
        SELECT backend_name, size_bytes FROM cleanup_dlq
    ) rows
    GROUP BY backend_name
) queued
WHERE queued.backend_name = q.backend_name;

CREATE VIEW backend_capacity AS
SELECT q.backend_name,
       q.bytes_limit,
       GREATEST(0, COALESCE(s.bytes_used, 0))::bigint AS bytes_used,
       q.orphan_bytes,
       (COALESCE(m.inflight, 0) + COALESCE(p.inflight, 0))::bigint AS inflight_bytes,
       CASE WHEN q.bytes_limit = 0 THEN NULL
            ELSE (q.bytes_limit
                  - GREATEST(0, COALESCE(s.bytes_used, 0))
                  - q.orphan_bytes
                  - COALESCE(m.inflight, 0)
                  - COALESCE(p.inflight, 0))::bigint
       END AS available_bytes,
       COALESCE(st.flags, 0)::bigint AS flags
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
