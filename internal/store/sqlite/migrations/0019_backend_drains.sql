-- A drain was held in the memory of the process that started it, so a restart
-- forgot it and nothing outside that process could see it. This table is the
-- drain's record: a row exists from the moment a drain starts until it is
-- cancelled or the backend is removed, and admission refuses a backend that
-- has one.
--
-- There is no foreign key to backend_quotas: the drained state has to outlive
-- that row when a backend is removed.

CREATE TABLE IF NOT EXISTS backend_drains (
    backend_name  TEXT PRIMARY KEY,
    state         TEXT NOT NULL CHECK (state IN ('draining', 'drained', 'failed')),
    objects_moved INTEGER NOT NULL DEFAULT 0,
    last_error    TEXT,
    started_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    finished_at   TEXT
);

-- What occupies each backend and whether it takes new writes, defined once for
-- every admission test and the quota tracker's baseline refresh.
--
-- In-flight is the parts of incomplete multipart uploads plus the intents of
-- writes still uploading: bytes on their way to a backend that no
-- object_locations row covers yet. available_bytes is NULL for a backend with
-- no ceiling (bytes_limit = 0). A backend with a drain record of any state
-- does not accept writes.
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
       d.backend_name IS NULL AS accepting_writes
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
LEFT JOIN backend_drains d ON d.backend_name = q.backend_name;
