This document covers the UI JSON API the built-in dashboard is served by. For the operational control plane, see the [Admin API](../admin-api/). For the S3-compatible API, see the [S3 API Coverage](../README.md#s3-api-coverage) section of the README.

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

Admin endpoints use a token rather than a session; see [Admin API](../admin-api/#authentication).

All JSON request bodies on admin and UI endpoints are limited to 1 MB.

**Object data caching:** When the optional in-memory cache is enabled, GET responses for eligible objects may be served from cache rather than from a backend. This is fully transparent to S3 API clients -- cached responses have the same headers, status codes, and body content as uncached responses. No client-side configuration or awareness is needed.

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
| `since` | No | RFC3339 timestamp -- only return entries after this time |
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

The admin API has moved to its own page: **[Admin API](../admin-api/)**.

Its endpoint reference is generated from the server's route table, so it cannot
drift from the code. That page also covers the parts a schema does not carry:
token authentication, the newline-delimited streaming mode, and the two-phase
confirmation a destructive backend purge requires.


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
