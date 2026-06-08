This document covers the JSON APIs provided by the orchestrator for programmatic access. For the S3-compatible API, see the [S3 API Coverage](../README.md#s3-api-coverage) section of the README.

## Authentication

### UI API

UI API endpoints use session cookie authentication. Obtain a session by posting credentials to the login endpoint:

```bash
curl -c cookies.txt -X POST \
  -d "admin_key=YOUR_KEY&admin_secret=YOUR_SECRET" \
  http://localhost:9000/ui/login

# Use the session cookie for subsequent requests
curl -b cookies.txt http://localhost:9000/ui/api/dashboard
```

Sessions are HMAC-SHA256 signed cookies with a 24-hour TTL.

### Admin API

Admin API endpoints use token authentication via the `X-Admin-Token` header:

```bash
curl -H "X-Admin-Token: YOUR_ADMIN_TOKEN" \
  http://localhost:9000/admin/api/status
```

The token is the `ui.admin_token` value from the configuration file. If `admin_token` is not set, it falls back to `ui.admin_key`. The CLI subcommand (`s3-orchestrator admin`) handles this automatically.

All JSON request bodies on admin and UI endpoints are limited to 1 MB.

**Object data caching:** When the optional in-memory cache is enabled, GET responses for eligible objects may be served from cache rather than from a backend. This is fully transparent to S3 API clients — cached responses have the same headers, status codes, and body content as uncached responses. No client-side configuration or awareness is needed.

## UI API Endpoints

All UI API endpoints are mounted under the configured UI path (default: `/ui`). They require an authenticated session cookie.

### GET /ui/api/dashboard

Returns the full dashboard data snapshot.

**Response:**

```json
{
  "BackendOrder": ["oci", "r2"],
  "QuotaStats": {
    "oci": {"BackendName": "oci", "BytesUsed": 5242880, "BytesLimit": 10737418240, "UpdatedAt": "..."},
    "r2": {"BackendName": "r2", "BytesUsed": 1048576, "BytesLimit": 10737418240, "UpdatedAt": "..."}
  },
  "ObjectCounts": {"oci": 42, "r2": 15},
  "ActiveMultipartCounts": {"oci": 0, "r2": 0},
  "UsageStats": {
    "oci": {"APIRequests": 1234, "EgressBytes": 5242880, "IngressBytes": 10485760}
  },
  "UsageLimits": {
    "oci": {"APIRequestLimit": 50000, "EgressByteLimit": 10737418240, "IngressByteLimit": 0}
  },
  "UsagePeriod": "2026-03",
  "TopLevelEntries": {
    "entries": [{"name": "my-bucket/", "isDir": true, "size": 6291456, "count": 57}],
    "hasMore": false,
    "nextCursor": ""
  }
}
```

### GET /ui/api/tree

Returns children of a directory prefix for the lazy-loaded file browser.

**Query parameters:**

| Parameter | Required | Description |
|-----------|----------|-------------|
| `prefix` | No | Directory prefix to list (e.g., `my-bucket/photos/`). Empty returns top-level entries. |
| `startAfter` | No | Cursor for pagination (value of `nextCursor` from previous response) |
| `maxKeys` | No | Maximum entries to return (1-200, default: 200) |

**Response:**

```json
{
  "entries": [
    {"name": "my-bucket/photos/2024/", "isDir": true, "size": 1048576, "count": 10},
    {"name": "my-bucket/photos/avatar.jpg", "isDir": false, "size": 51200, "count": 0}
  ],
  "hasMore": true,
  "nextCursor": "my-bucket/photos/avatar.jpg"
}
```

### GET /ui/api/logs

Returns buffered log entries from the in-memory ring buffer (last 5,000 entries).

**Query parameters:**

| Parameter | Required | Description |
|-----------|----------|-------------|
| `level` | No | Minimum severity: `DEBUG`, `INFO`, `WARN`, `ERROR` (default: all levels) |
| `since` | No | RFC3339 timestamp — only return entries after this time |
| `component` | No | Filter by `component` attribute value |
| `limit` | No | Maximum entries to return (default: all). When applied, returns the most recent N matching entries. |

**Response:**

```json
[
  {
    "time": "2026-03-02T14:30:00Z",
    "level": "INFO",
    "message": "Connected to PostgreSQL",
    "attrs": {"host": "db.example.com", "port": 5432, "component": "main"}
  }
]
```

### POST /ui/api/delete

Deletes a single object by key.

**Request body:**

```json
{"key": "my-bucket/path/to/file.txt"}
```

**Response (success):**

```json
{"ok": true}
```

**Response (error):**

```json
{"error": "failed to delete object: ..."}
```

### POST /ui/api/delete-prefix

Deletes all objects under a given key prefix.

**Request body:**

```json
{"prefix": "my-bucket/photos/vacation/"}
```

**Response (success):**

```json
{"ok": true, "deleted": 42}
```

**Response (partial failure):**

```json
{"error": "5 of 47 deletes failed", "deleted": 42}
```

### POST /ui/api/upload

Uploads a file via multipart form data. Maximum upload size is 512 MiB.

**Request:**

```bash
curl -b cookies.txt -X POST \
  -F "key=my-bucket/path/to/file.txt" \
  -F "file=@localfile.txt" \
  http://localhost:9000/ui/api/upload
```

The `key` must start with a configured virtual bucket name (e.g., `my-bucket/`).

**Response (success):**

```json
{"ok": true, "etag": "\"abc123...\""}
```

### GET /ui/api/download

Downloads a single object by key. The response streams the object body with appropriate headers for the browser to trigger a save dialog.

**Request:**

```bash
curl -b cookies.txt -OJ \
  "http://localhost:9000/ui/api/download?key=my-bucket/path/to/file.txt"
```

The `key` must start with a configured virtual bucket name.

**Response:** Binary object content with `Content-Disposition: attachment`, `Content-Type`, and `Content-Length` headers.

### POST /ui/api/rebalance

Triggers an on-demand rebalance in the background. Returns immediately with 202 Accepted. Poll the status endpoint for results.

**Request:** No body required.

**Response (202):**

```json
{"status": "started"}
```

**Response (409):** Returned if a rebalance is already running.

```json
{"error": "rebalance already running"}
```

### GET /ui/api/rebalance/status

Returns the status of the most recent rebalance operation.

**Response:**

```json
{"status": "running"}
{"status": "done", "ok": true, "moved": 5}
{"status": "error", "error": "rebalance failed"}
{"status": "idle"}
```

### POST /ui/api/clean-excess

Removes over-replicated copies in the background. Returns immediately with 202 Accepted. Poll the status endpoint for results.

**Request:** No body required.

**Response (202):**

```json
{"status": "started"}
```

**Response (409):** Returned if cleanup is already running.

```json
{"error": "cleanup already running"}
```

### GET /ui/api/clean-excess/status

Returns the status of the most recent cleanup operation.

**Response:**

```json
{"status": "running"}
{"status": "done", "ok": true, "removed": 3}
{"status": "error", "error": "cleanup failed"}
{"status": "idle"}
```

### POST /ui/api/sync

Imports pre-existing objects from a backend's S3 bucket into the database.

**Request body:**

```json
{"backend": "oci", "bucket": "my-bucket"}
```

Both `backend` (a configured backend name) and `bucket` (a configured virtual bucket name) are required.

**Response (success):**

```json
{"ok": true, "imported": 150, "skipped": 42}
```

## Admin API Endpoints

All admin API endpoints are mounted under `/admin/api/`. They require the `X-Admin-Token` header.

### GET /admin/api/status

Returns backend health, quota usage, object counts, and monthly usage stats.

**Response:**

```json
{
  "db_healthy": true,
  "backends": [
    {
      "name": "oci",
      "bytes_used": 5242880,
      "bytes_limit": 10737418240,
      "orphan_bytes": 0,
      "object_count": 42,
      "api_requests": 1234,
      "egress_bytes": 5242880,
      "ingress_bytes": 10485760
    }
  ],
  "usage_period": "2026-03"
}
```

### GET /admin/api/workers

Returns a snapshot of every registered background service's last-tick
health. Use this during incidents to distinguish "worker is running
but every tick fails" from "worker has not run". Returns `503` when
the deployment runs in proxy-only mode and no worker pool is wired.

**Response:**

```json
{
  "workers": [
    {
      "name": "cleanup_queue",
      "last_success": "2026-05-12T18:42:01Z",
      "consecutive_failures": 0
    },
    {
      "name": "replicator",
      "last_success": "2026-05-12T18:30:14Z",
      "last_failure": "2026-05-12T18:45:14Z",
      "last_error": "connection refused",
      "consecutive_failures": 3
    }
  ]
}
```

Fields:

- `name` — registration name, matches the snake_case slug on the scoped
  logger (e.g. `cleanup_queue`, `replicator`, `over_replication_cleanup`).
- `last_success` — RFC 3339 timestamp of the most recent successful
  tick. Omitted before the first success.
- `last_failure` — most recent failed tick. Omitted before the first
  failure. Stays set after recovery so operators can see how long ago
  the service was last failing.
- `last_error` — error string from the most recent failure. Cleared on
  the next success.
- `consecutive_failures` — count of back-to-back failed ticks since
  the last success. Resets to 0 on success.

The same data is exposed via Prometheus as
`s3o_worker_last_success_timestamp_seconds`,
`s3o_worker_consecutive_failures`, and `s3o_worker_ticks_total` so
alerting can run without scraping this endpoint.

### GET /admin/api/object-locations

Returns all copies of an object across backends.

**Query parameters:**

| Parameter | Required | Description |
|-----------|----------|-------------|
| `key` | Yes | Full object key including bucket prefix (e.g., `my-bucket/path/to/file.txt`) |

**Response:**

```json
{
  "key": "my-bucket/path/to/file.txt",
  "locations": [
    {"ObjectKey": "my-bucket/path/to/file.txt", "BackendName": "oci", "SizeBytes": 51200, "CreatedAt": "2026-01-15T10:30:00Z"},
    {"ObjectKey": "my-bucket/path/to/file.txt", "BackendName": "r2", "SizeBytes": 51200, "CreatedAt": "2026-01-15T10:35:00Z"}
  ]
}
```

### GET /admin/api/cleanup-queue

Returns the cleanup queue depth and pending items (up to 50).

**Response:**

```json
{
  "depth": 3,
  "items": [
    {"ID": 1, "BackendName": "oci", "ObjectKey": "my-bucket/old-file.txt", "Reason": "delete_failed", "Attempts": 2, "SizeBytes": 51200}
  ]
}
```

### POST /admin/api/usage-flush

Forces an immediate flush of usage counters to the database. Flushes from Redis when active, otherwise from local in-memory counters.

**Request:** No body required.

**Response:**

```json
{"status": "flushed"}
```

### POST /admin/api/usage-reconcile

Recomputes each backend's `bytes_used` from the authoritative object ledger (`SUM(object_locations.size_bytes)`), correcting drift in the incrementally maintained quota counter. The periodic reconcile pass runs this automatically; this endpoint forces it on demand.

**Request:** No body required.

**Response:** `adjustments` maps each corrected backend to the byte delta applied (negative when the counter was reduced); backends already in agreement are omitted.

```json
{"status": "reconciled", "adjustments": {"e2": -162801340}}
```

### POST /admin/api/replicate

Triggers one replication cycle. Returns immediately if replication is not configured or factor is 1.

**Request:** No body required.

**Response:**

```json
{"status": "ok", "copies_created": 5}
```

Or if replication is not configured:

```json
{"status": "skipped", "copies_created": 0, "reason": "replication not configured or factor <= 1"}
```

### GET /admin/api/over-replication

Returns the current replication factor and count of over-replicated objects.

**Response:**

```json
{"factor": 2, "pending": 15}
```

### POST /admin/api/over-replication

Triggers an immediate over-replication cleanup pass.

**Query parameters:**

| Parameter | Required | Description |
|-----------|----------|-------------|
| `batch_size` | No | Override the configured batch size for this run |

**Request:** No body required.

**Response:**

```json
{"status": "ok", "copies_removed": 5}
```

Or if replication is not configured:

```json
{"status": "skipped", "copies_removed": 0, "reason": "replication not configured or factor <= 1"}
```

### GET /admin/api/log-level

Returns the current runtime log level.

**Response:**

```json
{"level": "info"}
```

### PUT /admin/api/log-level

Changes the runtime log level without restart or SIGHUP.

**Request body:**

```json
{"level": "debug"}
```

Valid levels: `debug`, `info`, `warn`, `error`.

**Response:**

```json
{"level": "debug"}
```

### POST /admin/api/backends/{name}/drain

Starts draining a backend. All objects are migrated to other backends in the background. The backend is immediately excluded from new writes.

**Response (202 Accepted):**

```json
{"status": "drain started", "backend": "oci"}
```

**Error responses:**

```json
{"error": "backend \"oci\" not found"}
{"error": "backend \"oci\" is already draining"}
```

### GET /admin/api/backends/{name}/drain

Returns the current state of a drain operation.

**Response (active drain):**

```json
{
  "active": true,
  "objects_remaining": 150,
  "bytes_remaining": 52428800,
  "objects_moved": 42
}
```

**Response (no drain active):**

```json
{
  "active": false,
  "objects_remaining": 0,
  "bytes_remaining": 0,
  "objects_moved": 0
}
```

**Response (completed with error):**

```json
{
  "active": false,
  "objects_remaining": 0,
  "bytes_remaining": 0,
  "objects_moved": 100,
  "error": "context canceled"
}
```

### DELETE /admin/api/backends/{name}/drain

Cancels an active drain. Objects already moved are not rolled back.

**Response:**

```json
{"status": "drain cancelled", "backend": "oci"}
```

**Error response:**

```json
{"error": "backend \"oci\" is not draining"}
```

### DELETE /admin/api/backends/{name}

Removes all database records for a backend. This is destructive.

**Query parameters:**

| Parameter | Required | Description |
|-----------|----------|-------------|
| `purge` | No | Set to `true` to also delete objects from the backend's S3 storage |

**Response:**

```json
{"status": "backend removed", "backend": "oci"}
```

**Error responses:**

```json
{"error": "backend \"oci\" is currently draining, cancel the drain first"}
{"error": "failed to delete backend data: ..."}
```

### POST /admin/api/encrypt-existing

Encrypts all unencrypted objects in-place. Requires encryption to be enabled in the config. Each object is downloaded, encrypted, and re-uploaded to the same backend, counting as 2 API calls plus egress and ingress against the backend's usage quota.

**Response:**

```json
{"status": "complete", "encrypted": 1423, "failed": 0, "total": 1423}
```

### POST /admin/api/decrypt-existing

Decrypts all encrypted objects back to plaintext. Requires encryption to be enabled in the config (the key provider is needed to unwrap DEKs). Each object is downloaded, decrypted, and re-uploaded as plaintext, counting as 2 API calls plus egress and ingress against the backend's usage quota.

**Response:**

```json
{"status": "complete", "decrypted": 1423, "failed": 0, "total": 1423}
```

### POST /admin/api/rotate-encryption-key

Re-wraps all DEKs encrypted with a specific key ID using the current master key. This is a metadata-only operation — no object data is re-uploaded.

**Request body:**

```json
{"old_key_id": "config-0"}
```

**Response:**

```json
{"status": "complete", "rotated": 1423, "failed": 0, "total": 1423}
```

### POST /admin/api/scrub

Triggers an on-demand integrity scrub cycle. Verifies stored content hashes against actual object data on backends.

**Request body (optional):**

```json
{"batch_size": 500}
```

**Response:**

```json
{"status": "complete", "checked": 500, "mismatches": 0, "errors": 2}
```

### POST /admin/api/hash-existing

Computes and stores content hashes for all objects that don't have one yet.

**Response:**

```json
{"status": "complete", "hashed": 423, "failed": 0, "total": 423}
```

## Error Responses

All endpoints return errors as JSON:

```json
{"error": "description of the error"}
```

Common HTTP status codes:

| Code | Meaning |
|------|---------|
| 400 | Bad request (missing required parameters, invalid JSON) |
| 401 | Unauthorized (missing or invalid token/session) |
| 405 | Method not allowed |
| 500 | Internal server error |
