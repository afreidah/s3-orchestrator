-- One row per shared usage batch applied to backend_usage and
-- backend_request_usage, written in the same transaction as its deltas, so a
-- batch whose staged copy outlived its commit is not applied a second time.

CREATE TABLE IF NOT EXISTS usage_flushes (
    flush_id   TEXT PRIMARY KEY,
    applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX IF NOT EXISTS idx_usage_flushes_applied_at ON usage_flushes(applied_at);
