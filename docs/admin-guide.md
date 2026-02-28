# S3 Orchestrator Admin Guide

This guide walks through deploying, configuring, and operating the S3 Orchestrator from scratch. For architecture and feature details, see the [README](../README.md). For client-side usage (AWS CLI, rclone, SDKs), see the [User Guide](user-guide.md).

## Prerequisites

- **PostgreSQL** — any recent version. The orchestrator auto-applies its schema on startup.
- **At least one S3-compatible storage backend** — OCI Object Storage, Backblaze B2, AWS S3, MinIO, Wasabi, etc. You need a bucket and access credentials on that backend.
- **The orchestrator binary** — a Docker image (via `make push VERSION=vX.Y.Z`), a `.deb` package (via `make deb VERSION=X.Y.Z`), or built from source (`make run`).

## Quickstart

Get a minimal single-bucket, single-backend orchestrator running in five steps.

### 1. Create a PostgreSQL database

```sql
CREATE USER s3orchestrator WITH PASSWORD 'changeme';
CREATE DATABASE s3orchestrator OWNER s3orchestrator;
```

The orchestrator creates its tables automatically on startup — no manual schema setup required.

### 2. Create a minimal config

```yaml
server:
  listen_addr: "0.0.0.0:9000"

buckets:
  - name: "my-files"
    credentials:
      - access_key_id: "AKID_MYFILES"
        secret_access_key: "changeme-replace-with-random-secret"

database:
  host: "localhost"
  port: 5432
  database: "s3orchestrator"
  user: "s3orchestrator"
  password: "changeme"
  ssl_mode: "disable"

backends:
  - name: "primary"
    endpoint: "https://namespace.compat.objectstorage.us-phoenix-1.oraclecloud.com"
    region: "us-phoenix-1"
    bucket: "my-actual-bucket"
    access_key_id: "backend-access-key"
    secret_access_key: "backend-secret-key"
    force_path_style: true
    quota_bytes: 21474836480  # 20 GB
```

Save this as `config.yaml`.

### 3. Start the orchestrator

```bash
# From source
s3-orchestrator -config config.yaml

# Or via Docker
docker run -v $(pwd)/config.yaml:/etc/s3-orchestrator/config.yaml -p 9000:9000 s3-orchestrator
```

### 4. Verify it's running

```bash
curl http://localhost:9000/health
# ok
```

### 5. Test with a quick upload/download

```bash
aws --endpoint-url http://localhost:9000 \
    s3 cp /etc/hostname s3://my-files/test.txt

aws --endpoint-url http://localhost:9000 \
    s3 cp s3://my-files/test.txt -
```

## Configuration Walkthrough

This section covers each config section in detail. See `config.example.yaml` for a complete template.

All config values support `${ENV_VAR}` expansion — the orchestrator calls `os.Expand` on the entire YAML file before parsing. Use this for secrets:

```yaml
database:
  password: "${DB_PASSWORD}"
```

### server

```yaml
server:
  listen_addr: "0.0.0.0:9000"    # required
  max_object_size: 5368709120     # 5 GB default
  backend_timeout: "30s"          # per-operation timeout for backend S3 calls
```

- `listen_addr` is the only required field.
- `max_object_size` caps single-PUT uploads. Larger objects should use multipart upload (most clients do this automatically).
- `backend_timeout` bounds individual S3 API calls to backends. Increase if you have slow backends or large objects.

### buckets

Each bucket defines a virtual namespace with one or more credential sets.

```yaml
buckets:
  - name: "app1-files"
    credentials:
      - access_key_id: "AKID_APP1"
        secret_access_key: "secret1"

  - name: "app2-files"
    credentials:
      - access_key_id: "AKID_APP2_WRITER"
        secret_access_key: "secret2"
      - access_key_id: "AKID_APP2_READER"
        secret_access_key: "secret3"
```

**Generating credentials:** Use `openssl rand` to produce random keys:

```bash
# Generate an access key ID (20 chars, uppercase + digits)
openssl rand -hex 10 | tr '[:lower:]' '[:upper:]'

# Generate a secret access key (40 chars, base64)
openssl rand -base64 30
```

**Validation rules:**
- Bucket names must not contain `/`.
- Bucket names must be unique across the config.
- Access key IDs must be globally unique across all buckets.
- Each bucket must have at least one credential set.
- Each credential needs either `access_key_id` + `secret_access_key` (SigV4) or `token` (legacy).

Multiple credentials on the same bucket let different services share a namespace with independent keys. This is useful when you want a writer service and a reader service accessing the same files.

### database

```yaml
database:
  host: "db.example.com"        # required
  port: 5432                     # default: 5432
  database: "s3orchestrator"     # required
  user: "s3orchestrator"         # required
  password: "${DB_PASSWORD}"
  ssl_mode: "require"            # default: require (use "disable" for local dev)
  max_conns: 10                  # default: 10
  min_conns: 5                   # default: 5
  max_conn_lifetime: "5m"        # default: 5m
```

The default is `require`. Set `ssl_mode: disable` only for local development without TLS.

Pool settings (`max_conns`, `min_conns`, `max_conn_lifetime`) control the pgx connection pool. The defaults are fine for most deployments. Increase `max_conns` if you're seeing connection wait times under high concurrency.

### routing_strategy

Controls how the orchestrator selects a backend when writing new objects.

```yaml
routing_strategy: "pack"       # "pack" or "spread" (default: pack)
```

- **pack** (default) — fills the first backend in config order until its quota is full, then overflows to the next. Best for stacking free-tier allocations sequentially.
- **spread** — places each object on the backend with the lowest utilization ratio (`bytes_used / bytes_limit`). Best for distributing storage evenly across backends.

Both strategies respect quota limits and usage limits — full or over-limit backends are always skipped.

### backends

Each backend is an S3-compatible storage service with its own credentials and optional quota.

```yaml
backends:
  - name: "oci"
    endpoint: "https://namespace.compat.objectstorage.us-phoenix-1.oraclecloud.com"
    region: "us-phoenix-1"
    bucket: "my-oci-bucket"
    access_key_id: "${OCI_ACCESS_KEY}"
    secret_access_key: "${OCI_SECRET_KEY}"
    force_path_style: true
    quota_bytes: 21474836480     # 20 GB
```

**Endpoint URLs by provider:**

| Provider | Endpoint format | `force_path_style` |
|----------|----------------|-------------------|
| OCI Object Storage | `https://<namespace>.compat.objectstorage.<region>.oraclecloud.com` | `true` |
| Backblaze B2 | `https://s3.<region>.backblazeb2.com` | `true` |
| AWS S3 | `https://s3.<region>.amazonaws.com` | `false` |
| MinIO | `http://<host>:9000` | `true` |
| Wasabi | `https://s3.<region>.wasabisys.com` | `true` |

**Quota:** Set `quota_bytes` to limit how much data a backend can hold. Set to `0` or omit for unlimited. Quota is tracked in PostgreSQL and updated atomically with every write/delete.

**Usage limits:** Optional monthly caps on API requests, egress, and ingress per backend:

```yaml
    api_request_limit: 20000     # monthly API calls (0 = unlimited)
    egress_byte_limit: 1073741824  # 1 GB monthly egress (0 = unlimited)
    ingress_byte_limit: 0        # unlimited ingress
```

When a backend exceeds a usage limit, writes overflow to the next eligible backend. Limits reset each month automatically.

**Unsigned payload:** By default, uploads stream directly to backends without buffering the entire body in memory. The AWS SDK normally buffers the request body to compute a SigV4 payload hash (SHA-256), but the orchestrator uses `UNSIGNED-PAYLOAD` to skip this. Body integrity is protected by TLS at the transport layer.

Set `unsigned_payload: false` on a backend to restore the buffering behavior, which computes the full payload hash before uploading. This is only useful if you have a specific compliance requirement for end-to-end payload integrity independent of TLS — in practice, all S3-compatible providers use HTTPS and the TLS layer provides equivalent protection.

```yaml
    unsigned_payload: false  # buffer uploads to compute SigV4 payload hash (default: true)
```

### telemetry

```yaml
telemetry:
  metrics:
    enabled: true
    path: "/metrics"             # default: /metrics
  tracing:
    enabled: false
    endpoint: "localhost:4317"   # OTLP gRPC endpoint
    insecure: true               # no TLS to collector
    sample_rate: 1.0             # 1.0 = trace everything
```

Metrics are served on the same port as the S3 API. Tracing exports spans via gRPC OTLP (e.g., to Tempo or Jaeger).

### circuit_breaker

The circuit breaker is always active. These settings tune its sensitivity.

```yaml
circuit_breaker:
  failure_threshold: 3           # consecutive DB failures before opening (default: 3)
  open_timeout: "15s"            # delay before probing recovery (default: 15s)
  cache_ttl: "60s"               # key→backend cache TTL during degraded reads (default: 60s)
  parallel_broadcast: false      # fan-out reads to all backends in parallel (default: false)
```

When the database is unreachable, the orchestrator enters degraded mode: reads broadcast to all backends (with caching), writes return `503`. The circuit automatically recovers when the database comes back.

By default, degraded reads try each backend sequentially. When `parallel_broadcast` is enabled, all backends are tried concurrently and the first success wins — reducing worst-case read latency from `N * backend_timeout` to roughly the fastest backend's response time. Enable this if read latency during outages is critical, but note that each parallel broadcast sends API requests to all backends simultaneously, which counts against monthly usage limits.

The other defaults are sensible for most deployments. Increase `cache_ttl` if you have many read-heavy clients and want fewer backend round-trips during outages.

### rebalance

Moves objects between backends to optimize storage distribution. Disabled by default — enabling it will generate egress/ingress traffic on your backends.

```yaml
rebalance:
  enabled: true
  strategy: "pack"               # "pack" or "spread" (default: pack)
  interval: "6h"                 # default: 6h
  batch_size: 100                # objects per run (default: 100)
  threshold: 0.1                 # min utilization spread to trigger (default: 0.1)
  concurrency: 5                 # parallel moves per run (default: 5)
```

- **pack** — fills backends in config order, consolidating free space onto the last backend. Good for maximizing free-tier allocations.
- **spread** — equalizes utilization ratios across all backends. Good for distributing load.

Object moves run concurrently within each batch, bounded by `concurrency`. Increase for faster rebalancing; decrease to reduce backend load.

### replication

Creates additional copies of objects on different backends for redundancy.

```yaml
replication:
  factor: 2                      # copies per object (default: 1 = no replication)
  worker_interval: "5m"          # replication cycle (default: 5m)
  batch_size: 50                 # objects per cycle (default: 50)
```

The replication factor must be `<= number of backends`. The worker runs once at startup to catch up on any pending replicas, then continues at the configured interval. Reads automatically fail over to replicas if the primary copy is unavailable.

### Cleanup Queue

The cleanup queue requires no configuration — it is always active. When any backend object deletion fails during normal operations (PutObject orphan cleanup, DeleteObject, multipart part cleanup, rebalancer, replicator), the failed deletion is automatically enqueued for retry.

The background worker runs every minute and retries with exponential backoff (1 minute to 24 hours). After 10 failed attempts, the item stops being retried but remains in the database for manual investigation.

**Monitoring:** Alert on `s3proxy_cleanup_queue_depth` staying elevated — this means orphaned objects are accumulating. Alert on `s3proxy_cleanup_queue_processed_total{status="exhausted"}` — these items need manual attention.

**Manual cleanup:** Inspect exhausted items and resolve manually:

```sql
-- View items that exceeded max retries
SELECT id, backend_name, object_key, reason, attempts, last_error, created_at
FROM cleanup_queue
WHERE attempts >= 10
ORDER BY created_at;

-- Reset an item for retry (e.g., after fixing the backend)
UPDATE cleanup_queue SET attempts = 0, next_retry = NOW() WHERE id = 123;

-- Remove an item you've resolved manually
DELETE FROM cleanup_queue WHERE id = 123;
```

### rate_limit

Per-IP token bucket rate limiting. Requests exceeding the limit receive `429 SlowDown`.

```yaml
rate_limit:
  enabled: true
  requests_per_sec: 100          # token refill rate (default: 100)
  burst: 200                     # max burst size (default: 200)
  trusted_proxies:               # CIDRs whose X-Forwarded-For is trusted
    - "10.0.0.0/8"
    - "172.16.0.0/12"
```

When `trusted_proxies` is configured, the orchestrator extracts the real client IP from the `X-Forwarded-For` header using rightmost-untrusted extraction: it walks the XFF chain from right to left, skipping addresses within trusted CIDRs, and uses the first untrusted address for rate limiting. If the direct peer is not in a trusted CIDR, `X-Forwarded-For` is ignored entirely to prevent spoofing. Without `trusted_proxies`, the direct connection IP is always used.

### ui

Built-in web dashboard for operational visibility. Disabled by default.

```yaml
ui:
  enabled: true
  path: "/ui"                # URL prefix (default: /ui)
```

### usage_flush

Controls how often in-memory usage counters are flushed to the database. When adaptive flushing is enabled, the interval shortens automatically when any backend approaches a usage limit, improving enforcement accuracy.

```yaml
usage_flush:
  interval: "30s"            # base flush interval (default: 30s)
  adaptive_enabled: true     # shorten interval when near limits (default: false)
  adaptive_threshold: 0.8    # usage ratio to trigger fast flush (default: 0.8)
  fast_interval: "5s"        # interval when near limits (default: 5s)
```

- `interval` — how often counters are flushed under normal conditions. Lower values reduce staleness but increase database writes.
- `adaptive_enabled` — when `true`, the flush interval drops to `fast_interval` whenever any backend's effective usage exceeds `adaptive_threshold` of its configured limit.
- `adaptive_threshold` — the ratio (0–1 exclusive) at which fast flushing kicks in. At `0.8`, a backend at 80% of any usage limit triggers the fast interval.
- `fast_interval` — must be less than `interval`. Used when adaptive flushing detects a backend near its limits.

### lifecycle

Automatically deletes objects whose key matches a prefix and whose age exceeds the configured expiration. Useful for temporary uploads, staging artifacts, or anything with a known retention period.

```yaml
lifecycle:
  rules:
    - prefix: "tmp/"
      expiration_days: 7
    - prefix: "uploads/staging/"
      expiration_days: 1
```

- `prefix` — key prefix to match (required, must be non-empty).
- `expiration_days` — delete objects older than this many days (required, must be > 0).
- Omit the `lifecycle` section or leave `rules` empty to disable lifecycle entirely.
- Rules are evaluated every hour by a background worker with an advisory lock.
- Deletions go through the standard `DeleteObject` path — all copies removed, quotas decremented, failed deletes enqueued to the cleanup queue.
- Hot-reloadable via `SIGHUP`.

When enabled, the dashboard is served at `{path}/` on the same port as the S3 API. A JSON API endpoint is also available at `{path}/api/dashboard`.

All dashboard responses include security headers (`X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: strict-origin-when-cross-origin`, `Content-Security-Policy`). The dashboard does not require authentication by default (same as `/health` and `/metrics`). If the orchestrator is publicly exposed, put it behind a reverse proxy with auth (e.g., oauth2-proxy via Traefik).

## Multi-Backend Configurations

### Single backend with quota

The simplest setup. One backend with a byte cap:

```yaml
backends:
  - name: "oci"
    endpoint: "https://namespace.compat.objectstorage.region.oraclecloud.com"
    region: "us-phoenix-1"
    bucket: "my-bucket"
    access_key_id: "${OCI_KEY}"
    secret_access_key: "${OCI_SECRET}"
    force_path_style: true
    quota_bytes: 21474836480     # 20 GB
```

### Multiple backends with quotas (pack routing)

Stack multiple free-tier allocations. With the default `routing_strategy: "pack"`, when one backend fills up, writes overflow to the next. Use `routing_strategy: "spread"` instead to distribute objects evenly by utilization ratio:

```yaml
backends:
  - name: "oci-free"
    endpoint: "https://namespace.compat.objectstorage.region.oraclecloud.com"
    region: "us-phoenix-1"
    bucket: "free-tier-bucket"
    access_key_id: "${OCI_KEY}"
    secret_access_key: "${OCI_SECRET}"
    force_path_style: true
    quota_bytes: 21474836480     # 20 GB (OCI free tier)

  - name: "b2-free"
    endpoint: "https://s3.us-west-002.backblazeb2.com"
    region: "us-west-002"
    bucket: "free-tier-bucket"
    access_key_id: "${B2_KEY}"
    secret_access_key: "${B2_SECRET}"
    force_path_style: true
    quota_bytes: 10737418240     # 10 GB (B2 free tier)
```

This gives you 30 GB of combined storage across two providers.

### Multiple backends without quotas (requires replication or spread)

When all backends are unlimited and using the default `pack` routing, only the first backend would receive writes. To distribute data, either set `replication.factor >= 2` to replicate across backends, or use `routing_strategy: "spread"` to distribute writes by utilization ratio.

```yaml
backends:
  - name: "oci"
    endpoint: "https://namespace.compat.objectstorage.region.oraclecloud.com"
    region: "us-phoenix-1"
    bucket: "bucket-a"
    access_key_id: "${OCI_KEY}"
    secret_access_key: "${OCI_SECRET}"
    force_path_style: true
    # no quota_bytes — unlimited

  - name: "aws"
    endpoint: "https://s3.us-east-1.amazonaws.com"
    region: "us-east-1"
    bucket: "bucket-b"
    access_key_id: "${AWS_KEY}"
    secret_access_key: "${AWS_SECRET}"
    # no quota_bytes — unlimited

replication:
  factor: 2
```

**Validation rule:** You cannot mix unlimited and quota-limited backends. Either all backends have `quota_bytes` set (quota routing) or all are unlimited (replication or spread routing required).

## Onboarding a New Tenant

To add a new application to the orchestrator:

1. **Generate credentials** for the new tenant:

   ```bash
   echo "Access Key: $(openssl rand -hex 10 | tr '[:lower:]' '[:upper:]')"
   echo "Secret Key: $(openssl rand -base64 30)"
   ```

2. **Add a bucket entry** to your config:

   ```yaml
   buckets:
     # ... existing buckets ...
     - name: "new-app"
       credentials:
         - access_key_id: "${NEW_APP_ACCESS_KEY}"
           secret_access_key: "${NEW_APP_SECRET_KEY}"
   ```

3. **Reload the configuration** by sending `SIGHUP` (`kill -HUP $(pidof s3-orchestrator)`) — or restart the orchestrator.

4. **Hand the client** four pieces of information:
   - Endpoint URL (e.g., `http://s3-orchestrator.service.consul:9000`)
   - Bucket name (e.g., `new-app`)
   - Access Key ID
   - Secret Access Key

5. **Point them to the [User Guide](user-guide.md)** for client setup instructions.

## Importing Existing Data

The `sync` subcommand imports objects from an existing backend bucket into the orchestrator's metadata database. Use this when bringing a bucket that already has data under orchestrator management.

### Dry run first

Always preview what would be imported before committing:

```bash
s3-orchestrator sync \
  --config config.yaml \
  --backend oci \
  --bucket my-files \
  --dry-run
```

### Run the import

```bash
s3-orchestrator sync \
  --config config.yaml \
  --backend oci \
  --bucket my-files
```

The `--bucket` flag specifies which virtual bucket the imported objects belong to. Keys are stored internally as `{bucket}/{key}`, so this determines the namespace.

### Partial import with --prefix

Import only objects under a specific key prefix:

```bash
s3-orchestrator sync \
  --config config.yaml \
  --backend oci \
  --bucket my-files \
  --prefix "photos/"
```

Objects already tracked in the database for that backend are automatically skipped. The command logs per-page progress and a final summary.

### Sync flags

| Flag | Default | Description |
|------|---------|-------------|
| `--config` | `config.yaml` | Path to configuration file |
| `--backend` | (required) | Backend name to sync from |
| `--bucket` | (required) | Virtual bucket name to assign to imported objects |
| `--prefix` | `""` | Only sync objects with this key prefix |
| `--dry-run` | `false` | Preview without writing to the database |

## Monitoring

### Web dashboard

When `ui.enabled` is `true`, the dashboard at `{path}/` shows a live snapshot of:

- **Backend quota** — bytes used/limit with progress bars per backend
- **Monthly usage** — API requests, egress, and ingress per backend with limits
- **Object tree** — interactive collapsible file browser. Buckets and directories are collapsed by default; click to expand. Each directory shows a rollup file count and total size.
- **Configuration** — virtual bucket names, write routing strategy, replication factor, rebalance status, rate limiting

The dashboard is server-rendered HTML. The object tree uses JavaScript for lazy-loaded directory expansion — directories fetch their children on click via the `/ui/api/tree` endpoint.

JSON endpoints at `{path}/api/dashboard` and `{path}/api/tree` return data for programmatic access or integration with other tools.

### Health endpoint

```bash
curl http://localhost:9000/health
```

Returns `ok` when the database is reachable, `degraded` when the circuit breaker is open. Always returns HTTP 200 (so the service stays in load balancer rotation during degraded mode).

### Grafana dashboard

A comprehensive Grafana dashboard is included at `grafana/s3-orchestrator.json`. Import it via Grafana's UI (Dashboards → Import → Upload JSON file) or provision it from disk. It expects a Prometheus datasource with UID `prometheus`.

The dashboard covers all emitted metrics across seven rows: overview stats, storage quotas, monthly usage, request performance, backend operations, health/reliability (circuit breaker, degraded mode, cleanup queue, rate limits), and background workers (replication, rebalancer, lifecycle, audit). The background workers row is collapsed by default.

### Key Prometheus metrics

If `telemetry.metrics.enabled` is `true`, metrics are exposed at `/metrics`. Key metrics to alert on:

| Metric | What to watch |
|--------|---------------|
| `s3proxy_quota_bytes_available{backend="..."}` | Alert when approaching 0 — backend is almost full |
| `s3proxy_circuit_breaker_state` | Alert when > 0 — database is unreachable (1=open, 2=half-open) |
| `s3proxy_replication_pending` | Alert when consistently > 0 — replicas are falling behind |
| `s3proxy_requests_total{status_code="5xx"}` | Alert on elevated 5xx rates |
| `s3proxy_degraded_write_rejections_total` | Writes being rejected due to degraded mode |
| `s3proxy_usage_limit_rejections_total` | Operations rejected by usage limits |
| `s3proxy_rate_limit_rejections_total` | Requests rejected by per-IP rate limiting |
| `s3proxy_cleanup_queue_depth` | Alert when consistently > 0 — orphaned objects are failing cleanup |
| `s3proxy_cleanup_queue_processed_total{status="exhausted"}` | Items that exceeded max retries — manual intervention needed |
| `s3proxy_audit_events_total{event="..."}` | Audit log volume by event type — useful for detecting unusual activity |

### Structured logs

All logs are JSON to stdout. Key fields: `msg`, `level`, `error`, `backend`, `operation`.

**Audit logs** are a subset of structured logs with `"audit": true`. Every S3 API request and significant internal operation emits an audit entry with a `request_id` for correlation. Filter audit entries in your log pipeline with a JSON query on the `audit` field.

Key audit events:

| Event | Source | Description |
|-------|--------|-------------|
| `s3.PutObject`, `s3.GetObject`, `s3.DeleteObjects`, etc. | HTTP layer | S3 API request with method, path, bucket, status, duration |
| `storage.PutObject`, `storage.GetObject`, `storage.DeleteObjects`, etc. | Storage layer | Backend operation with key, backend name, size |
| `rebalance.start`, `rebalance.move`, `rebalance.complete` | Rebalancer | Object redistribution runs |
| `replication.start`, `replication.copy`, `replication.complete` | Replicator | Replica creation runs |
| `storage.MultipartCleanup` | Multipart cleanup | Stale upload cleanup |
| `cleanup_queue.processed` | Cleanup queue | Orphaned object successfully deleted on retry |

Each S3 API request produces two correlated audit entries (HTTP-level and storage-level) sharing the same `request_id`. Internal operations (rebalance, replication) generate their own correlation IDs. The `request_id` also appears as a `s3proxy.request_id` attribute on OpenTelemetry spans.

Clients can supply their own correlation ID via the `X-Request-Id` request header; otherwise the orchestrator generates one. The ID is returned in the `X-Amz-Request-Id` response header.

## Common Operations

### Reloading configuration

Many settings can be updated without restarting the orchestrator by sending `SIGHUP`:

1. **Edit the config file** with your changes.
2. **Send SIGHUP** to the running process:
   ```bash
   kill -HUP $(pidof s3-orchestrator)
   ```
3. **Check the logs** to confirm the reload succeeded:
   ```
   {"level":"INFO","msg":"SIGHUP received, reloading configuration","path":"config.yaml"}
   {"level":"INFO","msg":"Configuration reload complete"}
   ```

**What takes effect immediately:**

- Bucket credentials (add/remove/rotate credentials without downtime)
- Rate limit settings (requests per second, burst)
- Backend quota limits (`quota_bytes`)
- Backend usage limits (`api_request_limit`, `egress_byte_limit`, `ingress_byte_limit`)
- Rebalance settings (strategy, interval, batch size, threshold, enable/disable)
- Replication settings (factor, worker interval, batch size)
- Usage flush settings (interval, adaptive enabled/threshold/fast interval)

**What requires a restart:**

- `server.listen_addr`, `database`, `telemetry`, `ui`, `routing_strategy`
- Backend structural changes (endpoint, S3 credentials, adding/removing backends)

If any of these fields change, the reload still proceeds for the reloadable settings, and warnings are logged:

```
{"level":"WARN","msg":"Config field changed but requires restart to take effect","field":"server.listen_addr"}
```

**If the config file is invalid**, the orchestrator keeps the current configuration entirely and logs the parse/validation error:

```
{"level":"ERROR","msg":"Config reload failed, keeping current config","error":"invalid config: ..."}
```

No partial reload happens — either all reloadable settings update, or none do.

### Adding a new backend

Add the backend to the `backends` list in your config and restart the orchestrator. Backend count changes are not reloadable — a restart is required. Quota limits are synced to the database on startup.

### Adjusting quotas

Change `quota_bytes` in the config and send `SIGHUP`. Quota limits are synced to the database on reload. Alternatively, restart the orchestrator — `SyncQuotaLimits` also runs on startup.

### Enabling replication after initial setup

Add a `replication` section with `factor > 1` and send `SIGHUP` (or restart). When restarting, the replication worker runs immediately at startup to begin creating copies of existing objects, then continues at the configured interval. With `SIGHUP`, the new factor takes effect on the next worker tick.

Remember: the replication factor cannot exceed the number of backends.

### Rotating client credentials

Update the credentials in the bucket config and send `SIGHUP`. The new credentials take effect immediately and old credentials stop working. Coordinate with the tenant to update their client configuration at the same time.

**Example: rotating credentials without downtime**

To perform a zero-downtime credential rotation, temporarily add both old and new credentials:

1. Add the new credential alongside the old one:
   ```yaml
   buckets:
     - name: "app1-files"
       credentials:
         - access_key_id: "OLD_KEY"
           secret_access_key: "old_secret"
         - access_key_id: "NEW_KEY"
           secret_access_key: "new_secret"
   ```
2. Send `SIGHUP` — both credentials now work.
3. Update the client to use the new credentials.
4. Remove the old credential from the config and send `SIGHUP` again.

## Deployment

### Docker

```bash
# Local build
make build

# Multi-arch build and push to registry with version tag
make push VERSION=vX.Y.Z
```

The `VERSION` is baked into the binary via `-ldflags` and displayed in the web UI and `/health` endpoint. Use versioned tags (not `latest`) to avoid Docker layer caching issues on orchestration platforms.

```bash
docker run -d \
  --name s3-orchestrator \
  -v /path/to/config.yaml:/etc/s3-orchestrator/config.yaml \
  -p 9000:9000 \
  -e DB_PASSWORD=secret \
  -e OCI_ACCESS_KEY=... \
  -e OCI_SECRET_KEY=... \
  s3-orchestrator
```

The default entrypoint is `s3-orchestrator -config /etc/s3-orchestrator/config.yaml`. Mount your config file to that path, or override the command to use a different location.

Environment variables referenced in the config via `${VAR}` syntax are expanded at startup, so pass secrets as `-e` flags or via your orchestration platform's secret injection.

The `listen_addr` in your config determines which port the process binds to inside the container — make sure your `-p` mapping matches.

### Debian Package (Systemd)

Build a `.deb` package for bare-metal or VM deployments:

```bash
# Build for host architecture
make deb VERSION=X.Y.Z

# Build for both amd64 and arm64
make deb-all VERSION=X.Y.Z

# Build and validate with lintian
make deb-lint VERSION=X.Y.Z
```

Install and configure:

```bash
sudo dpkg -i s3-orchestrator_X.Y.Z_amd64.deb

# Edit the config — set database, backends, buckets
sudo vim /etc/s3-orchestrator/config.yaml

# Set secrets as environment variables (referenced via ${VAR} in config)
sudo vim /etc/default/s3-orchestrator

# Start the service
sudo systemctl start s3-orchestrator

# Check status
sudo systemctl status s3-orchestrator
sudo journalctl -u s3-orchestrator -f
```

The package installs:

| Path | Purpose |
|------|---------|
| `/usr/bin/s3-orchestrator` | Binary |
| `/etc/s3-orchestrator/config.yaml` | Configuration (conffile, preserved on upgrade) |
| `/etc/default/s3-orchestrator` | Environment variables for `${VAR}` expansion in config |
| `/usr/lib/systemd/system/s3-orchestrator.service` | Systemd unit |
| `/var/lib/s3-orchestrator/` | Data directory |

The systemd unit runs as a dedicated `s3-orchestrator` user with filesystem hardening (`ProtectSystem=strict`, `ProtectHome=yes`, `NoNewPrivileges=yes`). The service is enabled on install but not started automatically, allowing configuration before first start.

**Config reload** works via systemd:

```bash
sudo systemctl reload s3-orchestrator
```

This sends `SIGHUP` to the process, reloading bucket credentials, quota limits, rate limits, and rebalance/replication settings without downtime. See [Reloading configuration](#reloading-configuration) for details on what is and isn't reloadable.

**Uninstall:**

```bash
# Remove the package (preserves config and data)
sudo apt remove s3-orchestrator

# Remove everything including config, data, and system user
sudo apt purge s3-orchestrator
```
