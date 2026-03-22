This guide walks through deploying, configuring, and operating the S3 Orchestrator from scratch. For architecture and feature details, see the [README](../README.md). For client-side usage (AWS CLI, rclone, SDKs), see the [User Guide](user-guide.md).

## Prerequisites

- **PostgreSQL** — any recent version. The orchestrator auto-applies its schema on startup.
- **At least one S3-compatible storage backend** — OCI Object Storage, Backblaze B2, AWS S3, MinIO, Wasabi, etc. You need a bucket and access credentials on that backend.
- **The orchestrator binary** — a Docker image (via `make push VERSION=vX.Y.Z`), a `.deb` package (via `make deb VERSION=X.Y.Z`), or built from source (`make run`).
- **Redis** (optional) — for shared usage counters in multi-instance deployments. See [usage_flush](#usage_flush) for details.

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
# {"status":"ok","instance":"hostname"}
```

### 5. Test with a quick upload/download

```bash
aws --endpoint-url http://localhost:9000 \
    s3 cp /etc/hostname s3://my-files/test.txt

aws --endpoint-url http://localhost:9000 \
    s3 cp s3://my-files/test.txt -
```

## Configuration Walkthrough

This section covers each config section in detail. See `packaging/config.yaml` for a complete template.

All config values support `${ENV_VAR}` expansion — the orchestrator calls `os.Expand` on the entire YAML file before parsing. Use this for secrets:

```yaml
database:
  password: "${DB_PASSWORD}"
```

### server

```yaml
server:
  listen_addr: "0.0.0.0:9000"    # required
  log_level: "info"               # debug, info, warn, error (default: info, reloadable via SIGHUP)
  max_object_size: 5368709120     # 5 GB default
  max_concurrent_requests: 0      # max concurrent S3 requests (0 = unlimited, default)
  # max_concurrent_reads: 0       # separate limit for GET/HEAD (0 = use global limit)
  # max_concurrent_writes: 0      # separate limit for PUT/POST/DELETE (0 = use global limit)
  # load_shed_threshold: 0        # active shedding threshold (0.0-1.0, 0 = disabled)
  # admission_wait: "0s"          # brief wait before rejection (0 = instant)
  backend_timeout: "30s"          # per-operation timeout for backend S3 calls
  read_header_timeout: "10s"      # max time to read request headers (default: 10s)
  read_timeout: "5m"              # max time to read entire request including body (default: 5m)
  write_timeout: "5m"             # max time to write response (default: 5m)
  idle_timeout: "120s"            # max time to wait for next request on keep-alive (default: 120s)
  shutdown_delay: "5s"            # delay before HTTP drain on SIGTERM (default: 0, no delay)
```

- `listen_addr` is the only required field.
- `max_object_size` caps single-PUT uploads. Larger objects should use multipart upload (most clients do this automatically).
- `max_concurrent_requests` limits the number of S3 requests processed simultaneously. When the limit is reached, new requests are rejected with `503 SlowDown` and `Retry-After: 1`. Set to 2-3x `database.max_conns` for load shedding. `0` disables the limit.
- `max_concurrent_reads` and `max_concurrent_writes` provide separate concurrency limits for reads (GET, HEAD) and writes (PUT, POST, DELETE). When both are set, they replace `max_concurrent_requests` with independent pools so write storms cannot starve reads.
- `load_shed_threshold` enables active load shedding. When in-flight requests exceed this fraction of pool capacity (e.g. `0.8`), new requests are probabilistically rejected before the hard limit, providing smooth degradation instead of a cliff.
- `admission_wait` adds a brief wait before rejecting when the semaphore is full (e.g. `50ms`). Smooths micro-bursts without adding latency during sustained overload. Default `0` means instant rejection.
- `backend_timeout` bounds individual S3 API calls to backends. Increase if you have slow backends or large objects.
- `read_header_timeout` protects against slow-read attacks that hold connections open by sending headers slowly. The 10-second default is generous for any legitimate client.
- `read_timeout` and `write_timeout` bound the total time for reading/writing entire requests and responses. The 5-minute defaults accommodate large object transfers.
- `idle_timeout` controls how long keep-alive connections stay open waiting for the next request.
- `shutdown_delay` adds a pause between marking the instance as not-ready and starting the HTTP drain on SIGTERM. Set this to ~5s in environments where service deregistration is asynchronous (Consul, Kubernetes) so load balancers stop routing before connections are closed. Default `0` means no delay.

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
  max_conns: 50                  # default: 50; size to 2-3x max_concurrent_requests
  min_conns: 5                   # default: 5
  max_conn_lifetime: "5m"        # default: 5m
```

The default is `require`. Set `ssl_mode: disable` only for local development without TLS.

Pool settings (`max_conns`, `min_conns`, `max_conn_lifetime`) control the pgx connection pool. Size `max_conns` to 2–3x your `max_concurrent_requests` setting — each S3 operation uses at least one database connection, and background workers hold additional connections during their tick. See [Performance Tuning — Connection Pool Sizing](performance-tuning.md#connection-pool-sizing) for detailed guidance.

### routing_strategy

Controls how the orchestrator selects a backend when writing new objects.

```yaml
routing_strategy: "pack"       # "pack" or "spread" (default: pack)
```

- **pack** (default) — fills the first backend in config order until its quota is full, then overflows to the next. Best for stacking free-tier allocations sequentially.
- **spread** — places each object on the backend with the lowest utilization ratio (`(bytes_used + orphan_bytes) / bytes_limit`). Best for distributing storage evenly across backends.

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

**Quota:** Set `quota_bytes` to limit how much data a backend can hold. Set to `0` or omit for unlimited. Quota is tracked in PostgreSQL and updated atomically with every write/delete. Note that multipart uploads do not reserve quota upfront — temporary parts consume backend storage without being counted against the quota until `CompleteMultipartUpload` records the final object size. A client uploading many large parts could temporarily exceed a backend's quota before completion.

**Usage limits:** Optional monthly caps on API requests, egress, and ingress per backend:

```yaml
    api_request_limit: 20000     # monthly API calls (0 = unlimited)
    egress_byte_limit: 1073741824  # 1 GB monthly egress (0 = unlimited)
    ingress_byte_limit: 0        # unlimited ingress
```

When a backend exceeds a usage limit, writes overflow to the next eligible backend. Limits reset each month automatically.

**Unsigned payload:** By default, uploads stream directly to backends without buffering the entire body in memory. The AWS SDK normally buffers the request body to compute a SigV4 payload hash (SHA-256), but the orchestrator uses `UNSIGNED-PAYLOAD` to skip this. Without streaming, large uploads (multipart completion, replication) can cause out-of-memory kills.

For HTTPS endpoints, unsigned payload is enabled by default. For plain HTTP endpoints, it is auto-disabled unless explicitly set — AWS S3 rejects unsigned payloads over HTTP, but most S3-compatible backends (MinIO, R2, etc.) accept them. Set `unsigned_payload: true` on HTTP backends to enable streaming:

```yaml
    unsigned_payload: true   # stream uploads without buffering (auto-enabled for HTTPS)
```

Set `unsigned_payload: false` to force payload hashing. This buffers the entire object in memory before uploading — only use this if you have a specific compliance requirement for end-to-end payload integrity independent of TLS.

**Disable checksum:** AWS SDK v2 defaults to sending streaming checksums (CRC64NVME) on uploads. Some S3-compatible providers — notably Google Cloud Storage — reject these with `SignatureDoesNotMatch`. Set `disable_checksum: true` on backends that don't support the AWS checksum headers:

```yaml
    disable_checksum: true   # required for GCS HMAC interoperability
```

This sets the SDK's `RequestChecksumCalculation` and `ResponseChecksumValidation` to `WhenRequired`, disabling automatic checksum injection without affecting SigV4 request signing.

**Strip SDK headers:** AWS SDK v2 adds headers (`amz-sdk-invocation-id`, `amz-sdk-request`, `accept-encoding`) and a query parameter (`x-id`) that are included in the SigV4 signed header set. Google Cloud Storage does not include these when verifying the signature, causing `SignatureDoesNotMatch` errors. Set `strip_sdk_headers: true` to remove them before request signing:

```yaml
    strip_sdk_headers: true   # required for GCS HMAC interoperability
```

For GCS backends, you typically need both `disable_checksum: true` and `strip_sdk_headers: true`:

```yaml
  - name: "gcs"
    endpoint: "https://storage.googleapis.com"
    region: "auto"
    bucket: "my-bucket"
    access_key_id: "GOOG..."
    secret_access_key: "..."
    force_path_style: true
    disable_checksum: true
    strip_sdk_headers: true
```

### telemetry

```yaml
telemetry:
  metrics:
    enabled: true
    path: "/metrics"             # default: /metrics
    # listen: "127.0.0.1:9091"  # serve on separate address (keeps /metrics off the public port)
  tracing:
    enabled: false
    endpoint: "localhost:4317"   # OTLP gRPC endpoint
    insecure: true               # no TLS to collector
    sample_rate: 1.0             # fraction of requests that generate traces (use 0.01–0.1 in production)
```

Metrics are served on the same port as the S3 API. Tracing exports spans via gRPC OTLP (e.g., to Tempo or Jaeger).

**Production sample rate guidance:** A `sample_rate` of 1.0 traces every request, which is appropriate for development and low-traffic deployments. For production workloads above ~100 RPS, reduce to 0.01–0.1 to avoid overwhelming the trace backend with storage, network, and CPU overhead. Metrics and logs are unaffected by sample rate.

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

### backend_circuit_breaker

Per-backend circuit breakers isolate failures at the individual backend level. When a backend's credentials expire or the provider becomes unreachable, the circuit opens after consecutive failures and the backend is excluded from request routing. A single probe request tests recovery after the timeout elapses. Disabled by default.

```yaml
backend_circuit_breaker:
  enabled: true
  failure_threshold: 5             # consecutive failures before opening (default: 5)
  open_timeout: "5m"               # delay before probing recovery (default: 5m)
```

Unlike the database circuit breaker, which triggers degraded mode for the entire system, backend circuit breakers affect only the individual backend. Reads fall back to other replicas, and writes route to other backends with available quota. No extra API calls are made — the breaker trips purely on organic traffic failures.

The `s3o_circuit_breaker_state{name="<backend>"}` metric tracks each backend's circuit state (0=closed, 1=open, 2=half-open). Alert on `> 0` for individual backends to detect credential or provider issues. Requires a restart to change (not hot-reloadable).

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
  concurrency: 5                 # parallel object replications per cycle (default: 5)
  unhealthy_threshold: "10m"     # grace period before replacing copies on circuit-broken backends (default: 10m)
```

The replication factor must be `<= number of backends`. The worker runs once at startup to catch up on any pending replicas, then continues at the configured interval. Reads automatically fail over to replicas if the primary copy is unavailable.

Replication is **asynchronous** — writes go to a single backend and the replicator creates additional copies in the background. When a client overwrites an existing key, all old copies (including replicas) are removed and a single new copy is written. The replication factor drops to 1 until the next replicator cycle creates the additional copies. If the single backend holding the new copy fails before replication runs, the new version of the object is at risk. For most workloads this window (up to `worker_interval`) is acceptable. Lowering `worker_interval` reduces the exposure at the cost of more frequent DB queries and backend I/O.

**Health-aware replication:** When backend circuit breakers are enabled, the replicator monitors backend health. If a backend's circuit breaker has been open longer than `unhealthy_threshold`, copies on that backend are treated as unavailable and replacement copies are created on healthy backends. This prevents sustained outages from silently reducing redundancy. The threshold prevents churn during brief transient failures. Set to `0` to disable health-aware replication (copies on down backends are still counted).

### Cleanup Queue

The cleanup queue is always active. The only tunable is the number of parallel deletions per worker tick:

```yaml
cleanup_queue:
  concurrency: 10                # parallel cleanup deletions per tick (default: 10)
```

When any backend object deletion fails during normal operations (PutObject orphan cleanup, DeleteObject, overwrite displaced copies, multipart part cleanup, rebalancer, replicator), the failed deletion is automatically enqueued for retry.

Each enqueued item tracks the object's `size_bytes`. On enqueue, the backend's `orphan_bytes` counter is incremented so that write routing and replication target selection account for the physically unreleased space. On successful cleanup, `orphan_bytes` is decremented. This prevents quota overcommitment during sustained backend outages.

The background worker runs every minute and retries with exponential backoff (1 minute to 24 hours). After 10 failed attempts, the item remains in the queue and `orphan_bytes` stays incremented — the space stays reserved until an operator resolves it. The worker query filters exhausted items automatically via a partial index.

**Monitoring:** Alert on `s3o_cleanup_queue_depth` staying elevated — this means orphaned objects are accumulating. Alert on `s3o_cleanup_queue_processed_total{status="exhausted"}` — these items need manual attention. Alert on `s3o_quota_orphan_bytes` — elevated values mean backends have significant physically unreleased space.

**Manual cleanup:** Inspect exhausted items and resolve manually:

```sql
-- View items that exceeded max retries
SELECT id, backend_name, object_key, reason, attempts, size_bytes, last_error, created_at
FROM cleanup_queue
WHERE attempts >= 10
ORDER BY created_at;

-- Reset an item for retry (e.g., after fixing the backend)
UPDATE cleanup_queue SET attempts = 0, next_retry = NOW() WHERE id = 123;

-- After manually deleting the orphaned object from the backend, decrement orphan_bytes and remove the item:
UPDATE backend_quotas SET orphan_bytes = orphan_bytes - (SELECT size_bytes FROM cleanup_queue WHERE id = 123) WHERE backend_name = (SELECT backend_name FROM cleanup_queue WHERE id = 123);
DELETE FROM cleanup_queue WHERE id = 123;
```

### rate_limit

Per-IP token bucket rate limiting. When enabled, rate limiting applies to both the S3 proxy and the admin API. Requests exceeding the limit receive `429 SlowDown`.

```yaml
rate_limit:
  enabled: true
  requests_per_sec: 100          # token refill rate (default: 100)
  burst: 200                     # max burst size (default: 200)
  cleanup_interval: "1m"         # stale entry eviction interval (default: 1m)
  cleanup_max_age: "5m"          # evict entries not seen within this window (default: 5m)
  trusted_proxies:               # CIDRs whose X-Forwarded-For is trusted
    - "10.0.0.0/8"
    - "172.16.0.0/12"
```

A background goroutine evicts per-IP entries not seen within `cleanup_max_age` every `cleanup_interval`. Under high source-IP cardinality (e.g., DDoS), the map can hold up to `cleanup_max_age` worth of unique IPs — tune both values down if memory pressure is a concern.

When `trusted_proxies` is configured, the orchestrator extracts the real client IP from the `X-Forwarded-For` header using rightmost-untrusted extraction: it walks the XFF chain from right to left, skipping addresses within trusted CIDRs, and uses the first untrusted address for rate limiting. If the direct peer is not in a trusted CIDR, `X-Forwarded-For` is ignored entirely to prevent spoofing. Without `trusted_proxies`, the direct connection IP is always used.

> **Multi-instance note:** Rate limits are enforced per-instance. Behind a load balancer with round-robin routing, the effective rate for a given client is `requests_per_sec * instance_count`. Divide your desired aggregate rate by the number of API instances when configuring.

### ui

Built-in web dashboard for operational visibility and management. Disabled by default. Requires authentication via an admin key/secret pair — sessions are HMAC-signed cookies with a 24-hour TTL.

```yaml
ui:
  enabled: true
  path: "/ui"                          # URL prefix (default: /ui)
  admin_key: "${UI_ADMIN_KEY}"         # access key for dashboard login
  admin_secret: "${UI_ADMIN_SECRET}"   # secret key — plaintext or bcrypt hash
  admin_token: "${UI_ADMIN_TOKEN}"     # separate token for admin API (defaults to admin_key)
  session_secret: "${UI_SESSION_SECRET}" # optional — for multi-instance session portability
  force_secure_cookies: true           # always set Secure flag on cookies (for behind TLS proxy)
```

Both `admin_key` and `admin_secret` are required when `enabled` is `true`. Generate them the same way as bucket credentials:

```bash
echo "Admin Key: $(openssl rand -hex 10 | tr '[:lower:]' '[:upper:]')"
echo "Admin Secret: $(openssl rand -base64 30)"
```

**Bcrypt-hashed secrets:** For bare-metal deployments where the config file is at rest on disk, you can store `admin_secret` as a bcrypt hash instead of plaintext. The orchestrator detects bcrypt hashes automatically (they start with `$2`). Generate one with:

```bash
htpasswd -nbBC 10 "" 'your-secret' | cut -d: -f2
```

Both plaintext and bcrypt secrets are fully supported — no config migration needed.

**Session secret:** By default, session keys are derived deterministically from `admin_secret` using HMAC-SHA256, so sessions survive restarts. For multi-instance deployments behind a load balancer, all instances sharing the same `admin_secret` will accept each other's sessions automatically.

Set `session_secret` to use a separate key for session derivation — useful when you want to rotate the session key independently of `admin_secret`, or when different instances need different admin secrets but shared sessions.

### usage_flush

Controls how often usage counters are flushed to the database. When adaptive flushing is enabled, the interval shortens automatically when any backend approaches a usage limit, improving enforcement accuracy.

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

> **Multi-instance note:** Without Redis, each instance accumulates usage counters in memory between flushes. With N instances, the enforcement margin near limits is up to `N * interval` worth of unaccounted operations. Adaptive flushing reduces this near limits but doesn't eliminate it. For tighter enforcement, configure [Redis shared counters](#redis) to eliminate the cross-instance blind spot entirely, or reduce `interval` and run fewer API instances.

### redis

Optional shared usage counters for multi-instance deployments. When configured, all instances share usage counters via Redis instead of tracking them independently in memory. This eliminates the cross-instance blind spot between PostgreSQL flushes.

```yaml
redis:
  address: "redis.example.com:6379"  # host:port (required when section is present)
  password: "${REDIS_PASSWORD}"       # AUTH password (omit for no auth)
  db: 0                               # Redis database number (default: 0)
  tls: false                          # enable TLS (default: false)
  key_prefix: "s3orch"                # namespace for multi-tenant Redis (default: s3orch)
  failure_threshold: 3                # consecutive failures before local fallback (default: 3)
  open_timeout: "15s"                 # delay before probing Redis recovery (default: 15s)
```

- `address` — required when the `redis` section is present. The orchestrator PINGs Redis on startup and fails hard if unreachable.
- `key_prefix` — namespaces all Redis keys. Use different prefixes if multiple orchestrator deployments share one Redis instance.
- `failure_threshold` and `open_timeout` — control the circuit breaker that falls back to local counters when Redis is unavailable.

When Redis is active, the usage flush service acquires a PostgreSQL advisory lock so only one instance performs the destructive `GETSET` + flush-to-PG operation. When Redis is in fallback (or not configured), each instance flushes independently without a lock.

Redis is not reloadable — changing Redis settings requires a restart.

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

### encryption

Server-side envelope encryption with chunked AES-256-GCM. When enabled, objects are encrypted before being stored on backends and decrypted transparently on read. Exactly one key source is required.

```yaml
encryption:
  enabled: true
  chunk_size: 65536                    # default: 64KB (range: 4KB–1MB, must be power of 2)
  master_key: "${ENCRYPTION_KEY}"      # base64-encoded 256-bit key
```

**Generating a master key:**

```bash
openssl rand -base64 32
```

**Key source options** — exactly one of the following must be set:

| Source | Config field | When to use |
|--------|-------------|-------------|
| Inline | `master_key` | Base64-encoded 256-bit key in config or env var. Simplest option. |
| File | `master_key_file` | Path to a file containing exactly 32 raw bytes. Good for bare-metal with config management. |
| Vault Transit | `vault` | Delegate key wrapping/unwrapping to HashiCorp Vault. Best for production with HSM-backed key management. |

**Vault Transit configuration:**

```yaml
encryption:
  enabled: true
  vault:
    address: "http://vault.service.consul:8200"
    token: "${VAULT_TOKEN}"
    key_name: "s3-orchestrator"
    mount_path: "transit"     # default: transit
```

The Vault Transit engine handles wrapping and unwrapping DEKs — the orchestrator never sees the master key material. The `key_name` must reference an existing key in the Transit engine.

**Key rotation support:**

When rotating to a new master key, move the old key to `previous_keys` so existing objects can still be decrypted:

```yaml
encryption:
  enabled: true
  master_key: "${NEW_ENCRYPTION_KEY}"         # new primary key
  previous_keys:
    - "${OLD_ENCRYPTION_KEY}"                 # old key, kept for unwrapping
```

After updating the config, call the `rotate-encryption-key` admin API to re-wrap all DEKs with the new key. See [Rotating encryption keys](#rotating-encryption-keys) below.

**Important notes:**
- Encryption is **not reloadable** — changing encryption settings requires a restart.
- The `chunk_size` must stay the same for the lifetime of the data. Changing it after objects are encrypted will make those objects unreadable.
- Encrypted objects are slightly larger than their plaintext (header + per-chunk overhead). The exact overhead is: 32 bytes (header) + 28 bytes per chunk (nonce + auth tag).

When enabled, the dashboard is served at `{path}/` on the same port as the S3 API.

All dashboard responses include security headers (`X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: strict-origin-when-cross-origin`, `Content-Security-Policy`). The dashboard requires authentication via the configured `admin_key`/`admin_secret` — unauthenticated requests are redirected to the login page (HTML) or receive `401` (API).

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

## CLI Subcommands

### version

Prints the binary version, Go version, and platform:

```bash
s3-orchestrator version
# s3-orchestrator v0.8.0 go1.26.0 linux/amd64
```

### validate

Validates a configuration file without starting the server. Exits 0 on success with a brief summary, or exits 1 with error details. Useful for CI pipelines or pre-deploy checks:

```bash
s3-orchestrator validate -config config.yaml
```

### admin

Operational CLI for inspecting and controlling a running instance. Reads `config.yaml` to discover the server address and admin token (`ui.admin_token`, falling back to `ui.admin_key`), then makes HTTP requests to the admin API.

```bash
s3-orchestrator admin [flags] <command>
```

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `-config` | `config.yaml` | Path to configuration file |
| `-addr` | from config | Override server address |

**Commands:**

```bash
# Show backend health, usage, and circuit breaker state
s3-orchestrator admin status

# List all copies of an object across backends
s3-orchestrator admin object-locations -key "my-bucket/path/to/file.txt"

# Show cleanup queue depth and pending items
s3-orchestrator admin cleanup-queue

# Force flush usage counters to the database
s3-orchestrator admin usage-flush

# Trigger one replication cycle (creates missing replicas)
s3-orchestrator admin replicate

# Show count of over-replicated objects
s3-orchestrator admin over-replication

# Clean over-replicated objects (remove excess copies)
s3-orchestrator admin over-replication --execute

# Clean with a custom batch size
s3-orchestrator admin over-replication --execute --batch-size 200

# View the current log level
s3-orchestrator admin log-level

# Change log level at runtime (no restart or SIGHUP needed)
s3-orchestrator admin log-level -set debug

# Start draining a backend (migrates all objects to other backends)
s3-orchestrator admin drain <backend-name>

# Check drain progress
s3-orchestrator admin drain-status <backend-name>

# Cancel an active drain (objects already moved are not rolled back)
s3-orchestrator admin drain-cancel <backend-name>

# Remove a backend's database records (destructive, no data migration)
s3-orchestrator admin remove-backend <backend-name>

# Remove a backend AND delete its S3 objects
s3-orchestrator admin remove-backend <backend-name> --purge

# Encrypt all unencrypted objects in-place (requires encryption enabled)
s3-orchestrator admin encrypt-existing

# Re-wrap all DEKs encrypted with a specific key ID (key rotation)
s3-orchestrator admin rotate-encryption-key --old-key-id config-0
```

The admin API requires `ui.admin_token` (or `ui.admin_key` as fallback) to be set in the configuration. All requests are authenticated via the `X-Admin-Token` header.

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

- **Storage summary** — total bytes used/capacity across all backends
- **Backend quota** — bytes used/limit with progress bars per backend, object counts, active multipart uploads
- **Monthly usage** — API requests, egress, and ingress per backend with limits
- **Object tree** — interactive collapsible file browser. Buckets and directories are collapsed by default; click to expand. Each directory shows a rollup file count and total size.
- **Configuration** — virtual bucket names, write routing strategy, replication factor, rebalance status, rate limiting, encryption status
- **Logs** — recent structured log output from an in-memory ring buffer (last 5,000 entries). Filter by severity level and search by text. Logs are available immediately on page load — no need to SSH into the host.

The dashboard also provides management actions:

- **Upload** — upload files to any virtual bucket directly from the browser (up to 512 MiB per file)
- **Download** — download individual objects by clicking the download icon on any file in the tree
- **Delete** — delete individual objects by clicking the delete icon on any file in the tree
- **Rebalance** — trigger an on-demand rebalance using the configured strategy and settings
- **Clean Excess** — remove over-replicated copies that exceed the replication factor
- **Sync** — import pre-existing objects from a backend's S3 bucket into the proxy database. Select a backend and a virtual bucket — objects already in the database are skipped, and objects belonging to other virtual buckets are excluded.

The dashboard requires authentication. Users log in at `{path}/login` with the `admin_key` and `admin_secret` configured in the `ui` section. Sessions last 24 hours.

The dashboard is server-rendered HTML. The object tree uses JavaScript for lazy-loaded directory expansion — directories fetch their children on click via the `/ui/api/tree` endpoint.

JSON endpoints at `{path}/api/dashboard`, `{path}/api/tree`, and `{path}/api/logs` return data for programmatic access or integration with other tools. The logs endpoint accepts optional query parameters: `level` (minimum severity: DEBUG, INFO, WARN, ERROR), `since` (RFC3339 timestamp), `component`, and `limit`. Management endpoints (`{path}/api/delete`, `{path}/api/upload`, `{path}/api/rebalance`, `{path}/api/clean-excess`, `{path}/api/sync`) accept POST requests. The download endpoint (`{path}/api/download?key=...`) accepts GET requests. All API endpoints require authentication.

### Health endpoints

Two health endpoints serve different purposes:

**Liveness** (`/health`) — always returns HTTP 200. Use this for liveness probes (Consul checks, K8s livenessProbe). The service stays in rotation during temporary database outages.

```bash
curl http://localhost:9000/health
# {"status":"ok","instance":"hostname"}
# or {"status":"degraded","instance":"hostname"} when circuit breaker is open
```

**Readiness** (`/health/ready`) — returns HTTP 200 when the service is ready to handle traffic, HTTP 503 during startup (before migrations and backend initialization complete) and during shutdown drain. Use this for readiness probes (K8s readinessProbe, Nomad `on_update = "require_healthy"`).

```bash
curl http://localhost:9000/health/ready
# {"status":"ready","instance":"hostname"}      — 200
# {"status":"not ready","instance":"hostname"}  — 503
```

Both endpoints include an `instance` field with the hostname for identifying which instance responded in multi-instance deployments.

### Grafana dashboard

A comprehensive Grafana dashboard is included at `grafana/s3-orchestrator.json`. Import it via Grafana's UI (Dashboards → Import → Upload JSON file) or provision it from disk. It expects a Prometheus datasource with UID `prometheus`.

The dashboard covers all emitted metrics across nine rows: overview stats, storage quotas, monthly usage, request performance, backend operations, health/reliability (circuit breaker, degraded mode, cleanup queue, rate limits), background workers (replication, rebalancer, lifecycle, audit), over-replication cleanup (pending count, excess copies removed, errors, duration), and encryption (operations, errors, encrypt-existing, key rotation). The background workers, over-replication cleanup, and encryption rows are collapsed by default.

### Key Prometheus metrics

If `telemetry.metrics.enabled` is `true`, metrics are exposed at `/metrics`. Key metrics to alert on:

| Metric | What to watch |
|--------|---------------|
| `s3o_quota_bytes_available{backend="..."}` | Alert when approaching 0 — backend is almost full (accounts for orphan bytes) |
| `s3o_quota_orphan_bytes{backend="..."}` | Elevated values mean backends have physically unreleased space from pending cleanups |
| `s3o_circuit_breaker_state{name="database"}` | Alert when > 0 — database is unreachable (1=open, 2=half-open) |
| `s3o_circuit_breaker_state{name="<backend>"}` | Alert when > 0 — backend is unreachable or credentials expired |
| `s3o_replication_pending` | Alert when consistently > 0 — replicas are falling behind |
| `s3o_replication_health_copies_total` | Non-zero means health-aware replication is creating replacement copies for circuit-broken backends |
| `s3o_over_replication_pending` | Objects with more copies than the replication factor — should return to 0 after cleanup runs |
| `s3o_over_replication_errors_total` | Cleanup errors — indicates backends or metadata issues preventing excess copy removal |
| `s3o_requests_total{status_code="5xx"}` | Alert on elevated 5xx rates |
| `s3o_degraded_write_rejections_total` | Writes being rejected due to degraded mode |
| `s3o_usage_limit_rejections_total` | Operations rejected by usage limits |
| `s3o_rate_limit_rejections_total` | Requests rejected by per-IP rate limiting |
| `s3o_admission_rejections_total` | Requests rejected at the hard admission limit |
| `s3o_load_shed_total` | Requests probabilistically shed before the hard admission limit |
| `s3o_early_rejections_total` | Uploads rejected before body transmission (no backend capacity) |
| `s3o_cleanup_queue_depth` | Alert when consistently > 0 — orphaned objects are failing cleanup |
| `s3o_cleanup_queue_processed_total{status="exhausted"}` | Items that exceeded max retries — manual intervention needed |
| `s3o_audit_events_total{event="..."}` | Audit log volume by event type — useful for detecting unusual activity |
| `s3o_encryption_errors_total` | Any non-zero rate indicates encryption/decryption failures |
| `s3o_encrypt_existing_objects_total{status="error"}` | Failures during bulk encryption of existing data |
| `s3o_key_rotation_objects_total{status="error"}` | Failures during key rotation |
| `s3o_redis_fallback_active` | Alert when 1 — Redis is unavailable, using local counters |
| `s3o_redis_operations_total{operation,status}` | Track Redis operation success/error rates |

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

Each S3 API request produces two correlated audit entries (HTTP-level and storage-level) sharing the same `request_id`. Internal operations (rebalance, replication) generate their own correlation IDs. The `request_id` also appears as a `s3o.request_id` attribute on OpenTelemetry spans.

Clients can supply their own correlation ID via the `X-Request-Id` request header; otherwise the orchestrator generates one. The ID is returned in the `X-Amz-Request-Id` response header.

**Trace-to-log correlation** — JSON log output includes `trace_id` and `span_id` fields on every line emitted within an active OpenTelemetry span. Log aggregators like Grafana Loki can extract these fields to link directly from a log entry to the corresponding trace in Tempo, and vice versa.

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

- Log level (`server.log_level`) — can also be changed at runtime via `s3-orchestrator admin log-level -set debug`
- Bucket credentials (add/remove/rotate credentials without downtime)
- Rate limit settings (requests per second, burst)
- Backend quota limits (`quota_bytes`)
- Backend usage limits (`api_request_limit`, `egress_byte_limit`, `ingress_byte_limit`)
- Rebalance settings (strategy, interval, batch size, threshold, enable/disable)
- Replication settings (factor, worker interval, batch size, unhealthy threshold)
- Usage flush settings (interval, adaptive enabled/threshold/fast interval)

**What requires a restart:**

- `server.listen_addr`, server timeouts, `server.shutdown_delay`, `database`, `telemetry`, `ui`, `routing_strategy`, `encryption`, `redis`
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

### Draining a backend

Draining migrates all objects off a backend to other backends without data loss. Use this when decommissioning a backend but preserving all stored objects.

1. **Start the drain:**

   ```bash
   s3-orchestrator admin drain <backend-name>
   ```

   This immediately excludes the backend from new writes (PutObject and CreateMultipartUpload skip it) and begins migrating objects in batches of 100. Any in-progress multipart uploads on the backend are aborted first.

2. **Monitor progress:**

   ```bash
   s3-orchestrator admin drain-status <backend-name>
   ```

   Returns objects remaining, bytes remaining, objects moved so far, and whether the drain is still active. Poll this periodically until `active` is `false` and `objects_remaining` is `0`.

3. **Wait for completion.** The drain runs as a background goroutine. Each object is read from the source backend, written to the least-utilized eligible backend, and the database record is atomically swapped via compare-and-swap. Failed moves are logged but don't stop the drain.

4. **Remove the backend from config and restart:**

   ```bash
   # Edit config.yaml — remove the backend entry
   # Restart or redeploy the orchestrator
   ```

   After drain completes, `DeleteBackendData` cleans up remaining database records (usage, quota, cleanup queue) automatically. Removing the backend from config on restart prevents it from being re-initialized.

**Cancelling a drain:**

```bash
s3-orchestrator admin drain-cancel <backend-name>
```

Objects already moved are not rolled back. The backend becomes eligible for new writes again.

**Metrics to watch during drain:**

| Metric | Description |
|--------|-------------|
| `s3o_drain_active` | `1` while a drain is in progress |
| `s3o_drain_objects_moved_total` | Objects successfully migrated |
| `s3o_drain_bytes_moved_total` | Bytes migrated |

### Removing a backend

Removing deletes all database records for a backend. This is destructive — objects on that backend become inaccessible. Use `drain` first if you want to preserve data.

**Drop database records only** (objects remain on the backend's S3 storage):

```bash
s3-orchestrator admin remove-backend <backend-name>
```

**Drop database records AND delete S3 objects:**

```bash
s3-orchestrator admin remove-backend <backend-name> --purge
```

The `--purge` flag iterates all objects on the backend and deletes them from S3 before removing database records. This is best-effort — individual delete failures are logged but don't stop the operation.

After removing, edit the config to remove the backend entry and restart.

> **Note:** You cannot remove a backend that is currently draining. Cancel the drain first with `drain-cancel`.

### Important: update the config after drain or remove

Drain and remove state is held in memory only — it is **not** persisted to the database. This means:

- **If the service restarts with a drained/removed backend still in the config**, `SyncQuotaLimits` re-creates the backend's quota record and the backend is re-initialized as a fresh, empty backend eligible for new writes. No data is lost, but the decommissioned backend silently starts receiving traffic again.
- **If the service crashes during an active drain**, all drain progress is lost. The backend reverts to active on restart. You would need to restart the drain.
- **SIGHUP does not remove backends** — config reload only updates quota limits and usage limits. The in-memory backend map is set at startup and cannot be modified at runtime.

**Always remove the backend from the config file and restart (or redeploy) after a drain or remove operation completes.** The dashboard UI shows a pulsing "Draining" badge on backends with an active drain so you can monitor progress visually.

### Adjusting quotas

Change `quota_bytes` in the config and send `SIGHUP`. Quota limits are synced to the database on reload. Alternatively, restart the orchestrator — `SyncQuotaLimits` also runs on startup.

### Enabling replication after initial setup

Add a `replication` section with `factor > 1` and send `SIGHUP` (or restart). When restarting, the replication worker runs immediately at startup to begin creating copies of existing objects, then continues at the configured interval. With `SIGHUP`, the new factor takes effect on the next worker tick.

Remember: the replication factor cannot exceed the number of backends.

### Enabling encryption on existing data

If you enable encryption on an orchestrator that already has unencrypted objects, those objects remain unencrypted until you explicitly encrypt them. New objects are encrypted automatically; existing ones need the `encrypt-existing` admin API.

1. **Enable encryption** in the config and restart the orchestrator.

2. **Encrypt existing objects:**

   ```bash
   s3-orchestrator admin encrypt-existing
   ```

   This processes all unencrypted objects in batches of 100: downloads from the backend, encrypts, re-uploads the ciphertext (overwriting the plaintext), and updates the database record. The response shows progress:

   ```json
   {"status": "complete", "encrypted": 1423, "failed": 0, "total": 1423}
   ```

3. **Monitor** via the `s3o_encrypt_existing_objects_total` metric (labels: `success`, `error`).

Failed objects are logged individually and can be retried by calling `encrypt-existing` again — it only processes objects without encryption metadata.

### Rotating encryption keys

Key rotation re-wraps DEKs with a new master key without re-encrypting object data. This is a metadata-only operation and is fast regardless of object sizes.

1. **Generate a new master key:**

   ```bash
   openssl rand -base64 32
   ```

2. **Update the config** — set the new key as `master_key` and move the old key to `previous_keys`:

   ```yaml
   encryption:
     enabled: true
     master_key: "${NEW_ENCRYPTION_KEY}"
     previous_keys:
       - "${OLD_ENCRYPTION_KEY}"
   ```

3. **Restart the orchestrator** (encryption config is not reloadable).

4. **Re-wrap all DEKs:**

   ```bash
   s3-orchestrator admin rotate-encryption-key --old-key-id config-0
   ```

   The `old-key-id` identifies which key's DEKs to re-wrap. For inline config keys, the ID is `config-0` for the primary and `config-1`, `config-2`, etc. for previous keys in order. For file-based keys, the ID is `file-0`. For Vault Transit, it's the key name.

   The response shows progress:

   ```json
   {"status": "complete", "rotated": 1423, "failed": 0, "total": 1423}
   ```

5. **After all DEKs are re-wrapped**, you can optionally remove the old key from `previous_keys` and restart. Objects that were rotated no longer need the old key.

**Metrics to watch during rotation:**

| Metric | Description |
|--------|-------------|
| `s3o_key_rotation_objects_total{status="success"}` | DEKs successfully re-wrapped |
| `s3o_key_rotation_objects_total{status="error"}` | DEKs that failed re-wrapping |

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

### Nomad and Kubernetes

Production-ready manifests for both platforms are in [`deploy/`](../deploy/). Each includes a local demo script that stands up a complete environment in one command:

```bash
make kubernetes-demo   # k3d cluster with docker-compose backing services
make nomad-demo        # Nomad dev agent with docker-compose backing services
```

See [`deploy/README.md`](../deploy/README.md) for production deployment instructions, Vault integration, TLS/mTLS configuration, and Ingress setup.

### Multi-instance deployment

By default, every instance runs both the HTTP API and all background workers (`--mode=all`). For larger deployments, the `--mode` flag separates these roles:

| Mode | HTTP API | Background workers | Use case |
|------|----------|-------------------|----------|
| `all` (default) | Yes | All 6 services | Single-instance or small deployments |
| `api` | Yes | Usage flush only | Scale-out API instances behind a load balancer |
| `worker` | Health + metrics only | All 6 services | Dedicated background processing |

```bash
# API instance — serves S3 requests, no background workers
s3-orchestrator -config config.yaml -mode api

# Worker instance — runs rebalancer, replicator, cleanup, lifecycle
s3-orchestrator -config config.yaml -mode worker
```

**How it works:**

- **API instances** serve S3 requests, the web UI, and rate limiting. They run the usage-flush service to avoid losing counters on restart, but skip all advisory-locked background tasks.
- **Worker instances** run all 6 background services and expose `/health`, `/health/ready`, and `/metrics` for monitoring, but don't serve S3 traffic or the web UI.
- Background tasks that modify state (rebalancer, replicator, cleanup, lifecycle, multipart cleanup) use **PostgreSQL advisory locks** — only one instance cluster-wide executes each task per cycle. Running multiple worker instances is safe; extra instances simply skip cycles when the lock is held.

**Recommended topology:** N `api` instances behind a load balancer + 1–2 `worker` instances for redundancy. All instances share the same config file and PostgreSQL database.

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
