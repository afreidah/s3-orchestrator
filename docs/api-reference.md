# API Reference

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
curl -H "X-Admin-Token: YOUR_ADMIN_KEY" \
  http://localhost:9000/admin/api/status
```

The token is the `ui.admin_key` value from the configuration file. The CLI subcommand (`s3-orchestrator admin`) handles this automatically.

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

### POST /ui/api/rebalance

Triggers an on-demand rebalance across backends.

**Request:** No body required.

**Response:**

```json
{"ok": true, "moved": 5}
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
      "object_count": 42,
      "api_requests": 1234,
      "egress_bytes": 5242880,
      "ingress_bytes": 10485760
    }
  ],
  "usage_period": "2026-03"
}
```

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
    {"ID": 1, "BackendName": "oci", "ObjectKey": "my-bucket/old-file.txt", "Reason": "delete_failed", "Attempts": 2}
  ]
}
```

### POST /admin/api/usage-flush

Forces an immediate flush of in-memory usage counters to the database.

**Request:** No body required.

**Response:**

```json
{"status": "flushed"}
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
