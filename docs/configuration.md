---
title: "Configuration Walkthrough"
linkTitle: "Configuration"
weight: 21
---

This page walks every YAML block in the config, with examples and validation rules. Each subsection below has a corresponding per-topic doc with operational depth:

- [buckets](#buckets) → [docs/authentication.md](authentication.md) for SigV4 + credential semantics
- [database](#database) → [docs/database.md](database.md) for engine choice + schema + migrations
- [backends](#backends) → [docs/backends.md](backends.md) for routing, quotas, provider quick-ref
- [rebalance / replication](#rebalance) → [docs/replication.md](replication.md)
- [encryption / integrity](#encryption) → [docs/encryption.md](encryption.md)
- [cache](#cache) → [docs/backends.md](backends.md) (object data cache)
- [cleanup_queue / lifecycle / write_path](#cleanup-queue) → [docs/cleanup-and-lifecycle.md](cleanup-and-lifecycle.md)
- [telemetry](#telemetry) → [docs/monitoring.md](monitoring.md)
- [notifications](#notifications) → [docs/notifications.md](notifications.md)

---


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
  # spill_dir: "/var/lib/s3-orchestrator/spill"  # where bodies over 32 MiB buffer (default: OS temp dir)
  max_concurrent_requests: 0      # max concurrent S3 requests (0 with no read/write split = 1000)
  # max_concurrent_reads: 0       # separate limit for GET/HEAD (0 = use global limit)
  # max_concurrent_writes: 0      # separate limit for PUT/POST/DELETE (0 = use global limit)
  # load_shed_threshold: 0        # active shedding threshold (0.0-1.0, 0 = disabled)
  # admission_wait: "0s"          # brief wait before rejection (0 = instant)
  # max_header_bytes: 1048576     # max total request header size (default: 1 MiB)
  # max_header_value_count: 500   # max header values per request (default: 500)
  backend_timeout: "30s"          # per-operation timeout for backend S3 calls
  read_header_timeout: "10s"      # max time to read request headers (default: 10s)
  read_timeout: "5m"              # max time to read entire request including body (default: 5m)
  write_timeout: "5m"             # max time to write response (default: 5m)
  idle_timeout: "120s"            # max time to wait for next request on keep-alive (default: 120s)
  shutdown_delay: "5s"            # delay before HTTP drain on SIGTERM (default: 0, no delay)
```

- `listen_addr` is the only required field.
- `max_object_size` caps single-PUT uploads. Larger objects should use multipart upload (most clients do this automatically). PutObject materializes the body so a write can fail over to another backend, but only bodies up to 32 MiB are held in memory; anything larger spills to a temporary file. Peak memory from uploads is therefore bounded by `32 MiB x max_concurrent_writes` rather than by `max_object_size`, at the cost of disk for the spilled ones.
- `spill_dir` is where those spill files are written. The default is the OS temp directory, which is `/tmp` on Linux — and `/tmp` is tmpfs under the systemd default and in most container images, which means the spill that exists to keep large objects off the heap puts them straight back in RAM. **If your objects exceed 32 MiB, point this at real disk**, or size the host for `max_object_size x max_concurrent_writes` after all. The directory must exist at startup; the orchestrator refuses to start otherwise rather than failing the first large upload. Files are unlinked as soon as they are created, so nothing survives a crash and the directory needs no cleaning.
- `max_concurrent_requests` limits the number of S3 requests processed simultaneously. When the limit is reached, new requests are rejected with `503 SlowDown` and `Retry-After: 1`. Set to 2-3x `database.max_conns` for load shedding.

  **There is no unlimited mode.** Leaving all three of `max_concurrent_requests`, `max_concurrent_reads` and `max_concurrent_writes` at `0` applies a default of 1000 combined requests rather than disabling admission control. A deployment sized on the assumption that it has no cap is one that will meet `503 SlowDown` without knowing where it came from. Set the value explicitly if 1000 is not what you want.
- `max_concurrent_reads` and `max_concurrent_writes` provide separate concurrency limits for reads (GET, HEAD) and writes (PUT, POST, DELETE). When both are set, they replace `max_concurrent_requests` with independent pools so write storms cannot starve reads. **Background workers contend with HTTP writes, not reads** — cleanup, replication, rebalance, pending reaper, and over-replication acquire admission slots from the same pool sized to `max_concurrent_writes`. In merged mode (`max_concurrent_requests` only), every HTTP request and every background worker shares the single global pool. Size `max_concurrent_writes` to accommodate both peak HTTP write traffic and the worst-case overlap of background worker activity (typically the replication factor × replicator concurrency for the dominant case). See issue #835 for the design rationale.
- `load_shed_threshold` enables active load shedding. When in-flight requests exceed this fraction of pool capacity (e.g. `0.8`), new requests are probabilistically rejected before the hard limit, providing smooth degradation instead of a cliff.
- `admission_wait` adds a brief wait before rejecting when the semaphore is full (e.g. `50ms`). Smooths micro-bursts without adding latency during sustained overload. Default `0` means instant rejection.
- `max_header_bytes` caps the total size of a request's headers, and `max_header_value_count` caps how many header values one request may carry. Both default to the `net/http` values (1 MiB and 500), so leaving them unset gives the same behaviour as any other Go server. They exist for deployments that want a tighter bound than the stdlib's: a request with thousands of small headers costs the server memory and parsing time before any handler sees it, and the default 500 is well above what an S3 client sends. Exceeding either limit fails the request during header parsing, before routing or authentication. Both are non-reloadable - they are set on the listener at startup, so changing them needs a restart.
- `backend_timeout` bounds individual S3 API calls to backends. Increase if you have slow backends or large objects.
- `read_header_timeout` protects against slow-read attacks that hold connections open by sending headers slowly. The 10-second default is generous for any legitimate client.
- `read_timeout` and `write_timeout` bound the total time for reading/writing entire requests and responses. The 5-minute defaults accommodate large object transfers. Streaming admin operations are exempt from `write_timeout`: a scrub or compress-existing pass holds its NDJSON response open for the life of the run, which on a real fleet is longer than any timeout worth applying to a request meant to finish. Every other response is still bound by it, so there is no need to raise the value to accommodate a long pass.
- `idle_timeout` controls how long keep-alive connections stay open waiting for the next request.
- `shutdown_delay` adds a pause between marking the instance as not-ready and starting the HTTP drain on SIGTERM. Set this to ~5s in environments where service deregistration is asynchronous (Consul, Kubernetes) so load balancers stop routing before connections are closed. Default `0` means no delay.

### buckets

Each bucket defines a virtual namespace with one or more credential sets.

```yaml
buckets:
  - name: "app1-files"
    # max_multipart_uploads: 100  # optional; limit active multipart uploads (0 = unlimited)
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

SigV4 credentials also support presigned URLs automatically. Clients can generate time-limited presigned URLs using any AWS SDK presign client — no additional configuration is needed on the orchestrator side.

### database

The `driver` field selects between SQLite (embedded, zero-dependency) and PostgreSQL (required for multi-instance deployments). When `driver` is omitted, the orchestrator infers `postgres` if `host` is set, otherwise `sqlite`.

**SQLite (default for single-instance):**

```yaml
database:
  driver: sqlite
  path: "s3-orchestrator.db"     # default: s3-orchestrator.db
```

SQLite requires no external dependencies. The database file is created automatically on first start. Advisory lock-based leader election is replaced by a process-local mutex, so multi-instance deployments are not supported with SQLite.

**PostgreSQL (required for multi-instance):**

```yaml
database:
  driver: postgres
  host: "db.example.com"        # required
  port: 5432                     # default: 5432
  database: "s3orchestrator"     # required
  user: "s3orchestrator"         # required
  password: "${DB_PASSWORD}"
  ssl_mode: "require"            # default: require (use "disable" for local dev)
  max_conns: 50                  # default: 50; size to 2-3x max_concurrent_requests
  min_conns: 10                  # default: 10
  max_conn_lifetime: "5m"        # default: 5m
```

Pool settings (`max_conns`, `min_conns`, `max_conn_lifetime`) control the pgx connection pool. Size `max_conns` to 2-3x your `max_concurrent_requests` setting. See [Performance Tuning - Connection Pool Sizing](performance-tuning.md#connection-pool-sizing) for detailed guidance.

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

**Max object size:** Some providers impose per-object size limits (e.g. Supabase rejects uploads over 50 MB with 413 EntityTooLarge). Set `max_object_size` to prevent the orchestrator from routing writes, rebalance moves, or replication copies to a backend when the object exceeds the limit:

```yaml
    max_object_size: 52428800    # 50 MB (0 = unlimited)
```

**Usage limits:** Optional monthly caps on API requests, egress, and ingress per backend:

```yaml
    api_request_limit: 20000     # monthly API calls (0 = unlimited)
    egress_byte_limit: 1073741824  # 1 GB monthly egress (0 = unlimited)
    ingress_byte_limit: 0        # unlimited ingress
```

When a backend exceeds a usage limit, writes overflow to the next eligible backend. Limits reset each month automatically.

**Request pools:** providers meter operations in classes with separate allowances and disagree about the grouping, so a single `api_request_limit` either wastes the loose classes or blows the strict one. Name the grouping instead:

```yaml
    unmetered: [DeleteObject, DeleteObjects, AbortMultipartUpload]
    request_limits:
      - name: class_a
        operations: [PutObject, CopyObject, ListObjects, ListObjectsV2]
        limit: 5000
      - name: class_b
        operations: [GetObject, HeadObject, GetParts]
        limit: 50000
```

Pools are additive: an operation charges every pool containing it and needs headroom in all of them. `"*"` matches every operation not listed as `unmetered`, and `limit: 0` counts without refusing. `api_request_limit` still works and desugars to one `all` pool over `"*"`; setting both on a backend is rejected. See [backends.md](backends.md) for the full operation vocabulary.

**HTTP transport:** Each backend gets its own connection pool. The defaults suit a proxy serving concurrent client traffic alongside the rebalancer and replicator, but deployments range from a Raspberry Pi to a high-concurrency gateway, so they can be tuned per backend:

```yaml
    http:
      max_idle_conns: 100             # default: 100
      max_idle_conns_per_host: 100    # default: 100
      max_conns_per_host: 200         # default: 200
      response_header_timeout: 30s    # default: 30s
      force_http2: true               # default: true
```

Every field is optional and omitting the block leaves the backend dialled exactly as before. A value of `0` means "use the default" rather than "unlimited"; negative values are rejected at startup.

Set `force_http2: false` to make one backend negotiate HTTP/1.1. HTTP/2 against some proxy and gateway combinations collapses throughput by most of an order of magnitude — one report measured about 10 MiB/s through the orchestrator against about 74 MiB/s direct to the same backend, with HTTP/1.1 restoring 60-80 MiB/s. The interaction has not been isolated to any single component, so this is an escape hatch rather than a fix. It is the targeted form of the process-wide `GODEBUG=http2client=0`: only the backend you set it on is affected, and every other one keeps HTTP/2.

If throughput to one backend is far below what you measure against it directly, this is the first thing to try.

**Unsigned payload:** By default, uploads stream directly to backends without buffering the entire body in memory. The AWS SDK normally buffers the request body to compute a SigV4 payload hash (SHA-256), but the orchestrator uses `UNSIGNED-PAYLOAD` to skip this. Without streaming, large uploads (multipart completion, replication) can cause out-of-memory kills.

For HTTPS endpoints, unsigned payload is enabled by default. For plain HTTP endpoints, it is auto-disabled unless explicitly set — AWS S3 rejects unsigned payloads over HTTP, but most S3-compatible backends (MinIO, R2, etc.) accept them. Set `unsigned_payload: true` on HTTP backends to enable streaming:

```yaml
    unsigned_payload: true   # stream uploads without buffering (auto-enabled for HTTPS)
```

Set `unsigned_payload: false` to force payload hashing. This buffers the entire object in memory before uploading — only use this if you have a specific compliance requirement for end-to-end payload integrity independent of TLS.

Streaming never means an unknown length: every upload declares its size up front, so `Content-Length` is always sent and `Transfer-Encoding: chunked` is never used. That matters because SigV4 signs `content-length`, so a request that streams without it cannot validate — backends that require the header answer `411`, and backends that merely check the signature answer `403 SignatureDoesNotMatch`.

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

**Credential source:** `credential_source` selects how the orchestrator obtains credentials for the backend. Default is `static`, which uses the `access_key_id` / `secret_access_key` fields above. Set to `default_chain` to delegate to the AWS SDK's default credential chain (env vars, EC2 IMDS, SSO, `~/.aws/credentials`, STS assume-role). When `default_chain` is set, the two key fields must be omitted — leaving stale keys behind is rejected at validation so they cannot silently shadow the SDK-resolved credentials.

Use `default_chain` when:

- The orchestrator runs on an EC2 instance with an IAM role attached (IMDS-vended credentials rotate every ~6 hours and cannot be tracked by YAML).
- Local development uses SSO (`aws sso login`) instead of long-lived keys.
- You want the SDK to resolve credentials via STS assume-role chains.

```yaml
  - name: "aws-prod"
    endpoint: "https://s3.amazonaws.com"
    region: "us-east-1"
    bucket: "my-prod-bucket"
    credential_source: "default_chain"
    # access_key_id / secret_access_key intentionally omitted
```

Note: the config loader already expands `${ENV_VAR}` references at load time, so `access_key_id: ${AWS_ACCESS_KEY_ID}` covers the env-var case under `credential_source: static`. Use `default_chain` for credential sources the loader cannot reach (IMDS, SSO, STS) and for cases where refresh matters.

### telemetry

```yaml
telemetry:
  metrics:
    enabled: true
    path: "/metrics"             # default: /metrics
    # listen: "127.0.0.1:9091"  # serve on separate address (keeps /metrics off the public port)
    # require_listener: true    # default; fail startup if that address cannot be bound
  tracing:
    enabled: false
    endpoint: "localhost:4317"   # OTLP gRPC endpoint
    insecure: true               # no TLS to collector
    sample_rate: 1.0             # fraction of requests that generate traces (use 0.01–0.1 in production)
```

Metrics are served on the same port as the S3 API unless `listen` is set. Tracing exports spans via gRPC OTLP (e.g., to Tempo or Jaeger).

**`require_listener`** decides what happens when a configured `listen` address cannot be bound — a port conflict, a permission problem, an address that does not exist on the host. It defaults to `true`: startup fails and says why.

That default is deliberate. The alternative is an orchestrator that serves S3 traffic and reports itself healthy while Prometheus receives nothing, which looks correct from every angle except a graph nobody is watching yet. Both sockets are bound before the process reports ready, so the conflict surfaces as a startup error rather than after the load balancer has been told to send traffic.

Set it to `false` for development or embedded use, where the port may well be taken and best-effort metrics are fine. Startup then proceeds with a warning naming the address that failed. The setting only applies to a separate `listen` address; metrics served inline share the main socket and have nothing of their own to fail.

A metrics listener that dies *after* startup also stops the process, for the same reason.

**Production sample rate guidance:** A `sample_rate` of 1.0 traces every request, which is appropriate for development and low-traffic deployments. For production workloads above ~100 RPS, reduce to 0.01–0.1 to avoid overwhelming the trace backend with storage, network, and CPU overhead. Metrics and logs are unaffected by sample rate.

### circuit_breaker

The circuit breaker is always active. These settings tune its sensitivity.

```yaml
circuit_breaker:
  failure_threshold: 3           # consecutive DB failures before opening (default: 3)
  open_timeout: "15s"            # delay before probing recovery (default: 15s)
  cache_ttl: "60s"               # key→backend cache TTL during degraded reads (default: 60s)
  degraded_reads_enabled: true   # serve reads from backends while the DB is down (default: true)
  parallel_broadcast: false      # fan-out reads to all backends in parallel (default: false)
  degraded_broadcast_parallelism: 0 # cap concurrent probes during parallel broadcast; 0 = no cap (default: 0)
```

Set `degraded_reads_enabled: false` to fail reads with `503` during a database outage instead of broadcasting to the backends. That trades availability for cost and blast radius: a broadcast spends one API call per backend per read, so a long outage under read load can burn a metered backend's monthly allowance before anyone notices.

When the database is unreachable, the orchestrator enters degraded mode: reads broadcast to all backends (with caching), writes return `503`. The circuit automatically recovers when the database comes back.

By default, degraded reads try each backend sequentially. When `parallel_broadcast` is enabled, all backends are tried concurrently and the first success wins — reducing worst-case read latency from `N * backend_timeout` to roughly the fastest backend's response time. Enable this if read latency during outages is critical, but note that each parallel broadcast sends API requests to all backends simultaneously, which counts against monthly usage limits.

For fleets with many configured backends, set `degraded_broadcast_parallelism` to cap how many backends are probed at once. With a positive value, probes run as a rolling window: the first N launch immediately and each failure replenishes the slot with the next pending backend, so at most N goroutines (and at most N concurrent backend API calls / TLS handshakes) are in flight at any time. The default of `0` means fan out to every backend at once.

`backend_timeout` also bounds the cleanup of losing probes after a winner is declared, so a backend that hangs instead of honouring cancellation cannot strand a goroutine. The `s3o_degraded_broadcast_drain_timeout_total` metric increments whenever that bound is hit, surfacing a misbehaving backend.

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

The cleanup queue is always active. Tunables:

```yaml
cleanup_queue:
  concurrency: 10                # parallel cleanup deletions per tick (default: 10)
  claim_grace_period: 5m         # reclaim stale per-row claims older than this (default: 5m)
  multipart_stale_timeout: 24h   # abort multipart uploads older than this (default: 24h)
```

`multipart_stale_timeout` is consumed by the hourly `CleanupStaleMultipartUploads` sweep — uploads that have been open longer than this are aborted, their parts deleted from the backend, and the multipart rows removed. The default 24h matches the AWS S3 SDK's default abort behavior; lower it on backends with tight free-tier headroom to recover quota faster.

When any backend object deletion fails during normal operations (PutObject orphan cleanup, DeleteObject, overwrite displaced copies, multipart part cleanup, rebalancer, replicator), the failed deletion is automatically enqueued for retry.

Each enqueued item tracks the object's `size_bytes`. On enqueue, the backend's `orphan_bytes` counter is incremented so that write routing and replication target selection account for the physically unreleased space. On successful cleanup the row is removed and `orphan_bytes` is decremented in a single atomic CTE; a worker crash between the two operations cannot leave the counter inconsistent.

**Per-row claim pattern.** Every row carries `claimed_at` and `claimed_by` columns. When a worker tick fetches a batch it stamps each row with the current instance's identifier and timestamp, gated by `FOR UPDATE SKIP LOCKED` (Postgres) or SQLite's intrinsic single-writer serialisation. Two instances ticking concurrently always see disjoint row sets, so a connection death or rolling-deploy overlap that would otherwise let two workers process the same row is now structurally impossible. A claim older than `claim_grace_period` (default 5m) is reclaimable so a worker that died mid-process does not leave the row stuck; reclaims emit `s3o_cleanup_queue_stale_claims_recovered_total` and a `cleanup_queue.claim_recovered` audit event.

The background worker runs every minute and retries with exponential backoff (1 minute to 24 hours). Scheduling a retry clears the row's claim so it is immediately re-eligible for the next tick. After 10 failed attempts, the row is graduated to the `cleanup_dlq` table via `core.MoveCleanupToDLQ` (single transaction: read the row, insert it into `cleanup_dlq`, delete it from `cleanup_queue`). `orphan_bytes` is intentionally NOT decremented during the move because the backend object is still on disk. The DLQ entry retains the full row payload (key, backend, size, reason, last_error) plus an `original_id` correlation column so an operator can find the original queue entry.

**Monitoring:**

- `s3o_cleanup_queue_depth` staying elevated — orphaned objects are accumulating in the active queue.
- `s3o_cleanup_queue_processed_total{status="exhausted"}` — counter increments each time an item exhausts retries.
- `s3o_cleanup_queue_processed_total{status="success_absent"}` — counter increments each time a backend DELETE returned 404 and the row was dropped as idempotent success (the backend already agrees the object is gone). A sustained rate here is benign and just means upstream PUTs are silently failing somewhere; spikes are worth correlating with backend health.
- `s3o_cleanup_queue_stale_claims_recovered_total{backend}` — non-zero rate means a worker died mid-process or the grace period is too short for realistic worst-case processing time.
- `s3o_cleanup_dlq_depth > 0` — the DLQ holds at least one unrecoverable orphan; alerting here gives operators a direct signal instead of a counter delta.
- `s3o_cleanup_dlq_enqueued_total{backend}` — rate of graduations per backend; a single backend dominating means that backend's delete path is broken.
- `s3o_cleanup_enqueue_failures_total{backend,reason,stage}` — orphan-leak blind spot signal. The cleanup-queue itself is durable, but its *enqueue* path is best-effort: when a backend write succeeds and the DB is then unreachable, the orphan cannot be recorded in `cleanup_queue` and the only signal is this counter plus the matching `storage.OrphanEnqueueFailed` audit event. `stage="enqueue"` is the worst case (the cleanup-queue worker will never see this orphan); `stage="orphan_bytes"` means the row landed but the quota counter drifts. See the runbook below.
- `s3o_quota_orphan_bytes` — elevated values mean backends have significant physically unreleased space (DLQ entries are the long-tail contributors).

**Untracked-orphan recovery (cleanup enqueue failed during DB outage).** A non-zero rate of `s3o_cleanup_enqueue_failures_total{stage="enqueue"}` means at least one orphan exists on a backend with no `cleanup_queue` row. The cleanup-queue worker will not retry it; the storage will leak until reconciled. Recovery workflow:

1. Query the audit log for `event="storage.OrphanEnqueueFailed"` to enumerate the specific backend/key/size of each affected orphan during the outage window.
2. Once DB connectivity is restored, run `POST /admin/api/reconcile[?backend=name]`. The reconciler walks each backend's actual key list against `object_locations` using a bounded-memory sorted-merge and imports S3-only keys back onto the ledger, so the leaked bytes are accounted against the backend's quota again. It does not delete them - reconcile adopts, it does not clean. Use step 3 below or the manual workflow to remove objects you do not want. This is the same diff machinery that runs on the nightly reconcile interval.
3. If the audit log indicates more than a handful of failures, target the reconciler at the affected backends specifically rather than waiting for the next scheduled scan.

`stage="orphan_bytes"` failures do not need step 2 — the `cleanup_queue` row landed and the worker will eventually delete the object. The quota counter drift is reset when `backend_quotas.orphan_bytes` is reconciled against `cleanup_queue` (a periodic safety pass; not yet automated).

**Manual cleanup:** Inspect DLQ entries and resolve them deliberately. The bytes are still on the backend, so the workflow is *delete the object out-of-band, then write off the row + adjust orphan_bytes by the row's size*:

```sql
-- View unrecoverable orphans needing manual intervention
SELECT id, original_id, backend_name, object_key, reason, attempts,
       size_bytes, first_enqueued_at, moved_at, last_error
FROM cleanup_dlq
ORDER BY moved_at;

-- After confirming the object is gone (manual S3 delete, reconciler sweep, etc.):
BEGIN;
UPDATE backend_quotas
   SET orphan_bytes = GREATEST(0, orphan_bytes - (SELECT size_bytes FROM cleanup_dlq WHERE id = 42))
 WHERE backend_name = (SELECT backend_name FROM cleanup_dlq WHERE id = 42);
DELETE FROM cleanup_dlq WHERE id = 42;
COMMIT;

-- Or, to push a DLQ entry back through automatic retry (e.g. after fixing the backend):
INSERT INTO cleanup_queue (backend_name, object_key, reason, size_bytes, next_retry, attempts, last_error)
SELECT backend_name, object_key, reason, size_bytes, NOW(), 0, last_error
  FROM cleanup_dlq WHERE id = 42;
DELETE FROM cleanup_dlq WHERE id = 42;
```

### write_path

The write path can run in two modes. **Direct mode** (`enabled: false`) writes to the backend and commits the metadata immediately afterward; a crash between the two leaks bytes onto the backend with no DB record. **Pending-intent mode** (`enabled: true`, the default) inserts a row into `pending_objects` *before* the backend PUT and atomically deletes that row when the metadata commits — so a crash between the PUT and the commit leaves a recoverable intent the background reaper can resolve.

```yaml
write_path:
  pending_pattern:
    enabled: true        # default: true; PUT-before-COMMIT crash-recovery pattern
    reaper_tick: 1m      # how often PendingReaper sweeps unresolved intents (default: 1m)
    min_age: 5m          # only intents older than this are eligible (default: 5m) — avoids racing in-flight PUTs
    batch_size: 50       # rows claimed per tick (default: 50)
  multipart:
    enforce_min_part_size: true  # require every part but the last to be >= 5 MiB (default: true)
```

**`multipart.enforce_min_part_size`** is the one S3 multipart invariant that is optional. Part-number range, ordering, duplicate and ETag checks are always enforced; the 5 MiB floor is not, because a deployment with existing writers that split more finely than S3 allows needs it off to accept their manifests. Leave it on unless you have such a writer.

**How recovery works.** On every tick the `PendingReaper` worker (`internal/worker/pending.go`) claims a batch of `pending_objects` rows older than `min_age`, HEADs the backend at the recorded key, and resolves each one:

- **HEAD 200** → the backend received the bytes. Promote the intent to a committed `object_locations` row (`pending_reaper.promoted` audit event).
- **HEAD 404** → the backend never received the bytes. Drop the intent (`pending_reaper.dropped` audit event). No orphan exists.
- **Non-404 HEAD error** → leave the intent for the next tick. A sustained backend reachability problem here surfaces as `s3o_pending_intents_resolved_total{status="ambiguous"}`.
- **A later write for the same key already committed** → drop the intent as superseded (`pending_reaper.superseded`).

**Why `min_age` matters.** The reaper must not race the foreground write path; if `min_age` is too short the reaper can interrogate an intent whose backend PUT is still in flight and either prematurely commit it or churn `ambiguous` resolutions. The 5-minute default is generous; lower it only if you have measured the p99 PUT duration and accept the operational tradeoff.

**Monitoring:**

- `s3o_pending_intents_enqueued_total` — should track the PutObject rate closely.
- `s3o_pending_intents_resolved_total{status}` — `committed` is the happy path (synchronous commit succeeded); `promoted` + `dropped` are reaper resolutions; `ambiguous` is the alert.
- `s3o_pending_intents_depth` — gauge of unresolved intents. Alert when consistently above `batch_size` — the reaper is not keeping up (raise `batch_size`, lower `reaper_tick`, or add concurrency).
- Audit events: `pending_reaper.promoted` / `pending_reaper.dropped` / `pending_reaper.superseded`.

**When to disable.** Don't, unless you are running an embedded SQLite single-instance demo and trust the OS to flush. The pattern adds one DB write per PUT (cheap) and saves you from one entire class of write-path crash leak.

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
  session_secret: "${UI_SESSION_SECRET}" # required — HMAC key for session cookies
  force_secure_cookies: true           # always set Secure flag on cookies (for behind TLS proxy)
```

`admin_key`, `admin_secret`, and `session_secret` are all required when `enabled` is `true`. Generate credentials the same way as bucket credentials:

```bash
echo "Admin Key: $(openssl rand -hex 10 | tr '[:lower:]' '[:upper:]')"
echo "Admin Secret: $(openssl rand -base64 30)"
```

**Bcrypt-hashed secrets:** For bare-metal deployments where the config file is at rest on disk, you can store `admin_secret` as a bcrypt hash instead of plaintext. The orchestrator detects bcrypt hashes automatically (they start with `$2`). Generate one with:

```bash
htpasswd -nbBC 10 "" 'your-secret' | cut -d: -f2
```

Both plaintext and bcrypt secrets are fully supported — no config migration needed.

**Session secret:** Session keys are derived deterministically from `session_secret` using HMAC-SHA256, so sessions survive restarts. For multi-instance deployments behind a load balancer, all instances sharing the same `session_secret` will accept each other's sessions. Generate a value with:

```bash
openssl rand -hex 32
```

`session_secret` is independent of `admin_secret` — rotating the admin password does not invalidate active sessions, and vice versa.

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

A background health probe runs every 5 seconds while the breaker is open: it PINGs Redis and, on success, syncs the accumulated local-counter deltas back via an additive INCRBY pipeline (no DEL — keys from before the outage expire via TTL) and recloses the breaker. The breaker recovery is clean: the failure counter is zeroed so the system tolerates the configured `failure_threshold` of new transient errors before tripping again. No process restart is required after a Redis outage.

Redis is not reloadable — changing Redis settings requires a restart.

### lifecycle

Automatically deletes objects matching a rule's filter whose age exceeds the configured expiration. Useful for temporary uploads, staging artifacts, or anything with a known retention period.

```yaml
lifecycle:
  rules:
    - prefix: "tmp/"
      expiration_days: 7
    - prefix: "logs/"
      tags:
        env: staging
      expiration_days: 7
    - tags:
        scratch: "true"
      expiration_days: 1
```

- `prefix` — key prefix to match.
- `tags` — [tags](tagging.md) the object must carry, as key/value pairs. Every one must match, so a rule with a prefix and tags selects their intersection.
- `expiration_days` — delete objects older than this many days (required, must be > 0).
- At least one of `prefix` or `tags` is required; a rule with neither would expire the whole namespace and is refused at startup.
- Two rules may share a prefix when their tags differ. Two rules with the same filter are refused as a duplicate.
- The cutoff is measured from the object's creation time, not from when a tag was applied, so tagging an object older than the window makes it eligible immediately. See [Cleanup and lifecycle](cleanup-and-lifecycle.md#age-is-measured-from-object-creation).
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
    token_file: "/secrets/vault_token"  # optional; re-read on each renewal tick
    key_name: "s3-orchestrator"
    mount_path: "transit"     # default: transit
    ca_cert: "/etc/ssl/vault-ca.pem"    # optional; PEM CA bundle for verifying Vault's TLS
    renew_interval: "5m"      # how often the token is renewed / re-read (default: 5m)
```

The Vault Transit engine handles wrapping and unwrapping DEKs — the orchestrator never sees the master key material. The `key_name` must reference an existing key in the Transit engine.

Set `ca_cert` when Vault presents a certificate signed by a private CA; without it the system trust store is used. `token_file` suits Nomad or Kubernetes workload identity, where the token is rotated on disk by the platform: it is re-read on every `renew_interval` tick rather than only at startup.

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

### compression

At-rest compression. When enabled, objects are stored on backends as chunked zstd and served back as the bytes the client wrote: sizes, ETags and content hashes stay those of the logical object.

Every read, write, worker and recovery path honours this. It stays off by default; there is no way to compress objects already stored, so enabling it affects new writes only.

```yaml
compression:
  enabled: true
  level: "default"             # fastest, default, better, or best
  chunk_size: 1048576          # default: 1MB (range: 16KB-64MB)
  min_size: 4096               # objects smaller than this are stored uncompressed
  min_ratio: 0.95              # encoded objects above this fraction of the original are discarded
```

**Why the level is a name.** zstd collapses its numeric 1-19 range into four buckets, so levels 10 and 19 produce byte-identical output. A numeric setting would let you express a distinction the encoder discards. The four names are the four levels it actually implements:

| Level | Trades |
|-------|--------|
| `fastest` | Highest throughput, largest output |
| `default` | Compression speed has not yet collapsed and the ratio is within a few percent of the best |
| `better` | Slower writes for a modestly better ratio |
| `best` | Substantially slower writes; decompression speed is unaffected |

Decompression speed does not degrade as the level rises, so the trade is entirely on the write side.

**Why chunks.** Compression emits backreferences into earlier data, so decoding can only begin at a frame boundary. One frame per object gives a single entry point, meaning any range read has to fetch the whole stored object and discard the prefix. Splitting the object into independently decodable chunks gives one entry point per chunk, so a range read fetches roughly the bytes it asked for. The cost is a seek table and a small ratio penalty, which at the 1MB default is negligible.

Larger chunks compress marginally better and make a small range read pull more than it needs; smaller chunks do the reverse and grow the seek table. 

**Important notes:**
- Compression is **not reloadable** — changing any field requires a restart.
- `chunk_size` and `level` apply to newly written objects only. Existing objects carry their own layout in their seek table and stay readable after either changes.
- `min_size` exists because a small object can come out larger than it went in: every stored object carries frame headers and a seek table, and below a few kilobytes that overhead can exceed anything compression saves.
- `min_ratio` catches what `min_size` cannot: an object of any size whose content will not compress. The object is encoded, measured, and stored raw when the result is not at least this much smaller. Lower the value to demand a bigger saving before paying for a decode on every read; `1.0` keeps any saving at all.
- Compression runs before encryption, because ciphertext does not compress.

### integrity

SHA-256 content hashing for data integrity verification. When enabled, objects are checksummed on write and the hash is stored alongside the object location in PostgreSQL.

```yaml
integrity:
  enabled: true
  verify_on_read: true               # Hash-check every GET response as it streams
  verify_on_replicate: false         # Read each new replica back and hash-check it
  scrubber_interval: "6h"            # Background verification interval (0 = disabled)
  scrubber_batch_size: 100           # Objects per scrub cycle
```

**How it works:**

- **Write path:** SHA-256 is computed on the plaintext body (before encryption) and stored in `object_locations.content_hash`.
- **Read path (`verify_on_read`):** A `VerifyingReader` wraps the response body and computes the hash as data streams to the client. On mismatch at EOF, the corrupted copy is enqueued for cleanup.
- **Replication (`verify_on_replicate`):** Each new copy is read back from its target, decoded to plaintext, and checked against the source's hash before the ledger row that makes it count toward the replication factor is written. A copy whose hash disagrees is deleted and another target is tried.
- **Scrubber:** A background worker works through the copies least recently verified, reads them from their backend, decrypts if needed, and checks the hash. A copy that fails has its bytes discarded and its ledger row removed, so the replicator rebuilds it from a healthy copy. Each read counts against the backend's usage quota.
- **Backfill:** Objects written before integrity was enabled have no stored hash. Use `admin backfill-checksums` to read those objects and compute their hashes.

**What `verify_on_replicate` costs, and why it is off by default.** Hashing on write is nearly free, because the digest is computed during a buffering pass the write path already performs. Verifying a replica is not: it reads the whole copy back, so every replica costs its size in egress twice instead of once. Enabling integrity therefore does not enable this on its own. Turn it on when the window it closes matters to you - a corrupt copy counts toward the replication factor from the moment its row exists, and until the scrubber reaches it the object reads as fully replicated when it is not.

Only a hash that actually disagrees discards a copy. Three situations leave the copy in place and report it as unverified rather than rejecting it: the source carries no `content_hash` (written before integrity, or not yet backfilled), the target has no egress headroom left under its usage limit, or the read-back failed. A copy that cannot be checked is not a copy known to be bad, and discarding it would leave the object under-replicated for reasons that have nothing to do with its contents. Backends that are not read-after-write consistent make that last case routine.

A mismatch does not say which end is damaged. The copy is byte-for-byte identical to its source, so a source that has already rotted produces a faithful copy of bad bytes and every target disagrees the same way. Both backend names appear in the log; `admin scrub -key <key>` settles which one is wrong.

**Sizing the sweep.** `scrubber_interval` and `scrubber_batch_size` together decide how long a full pass takes: copies divided by batch size, times the interval. Verifying a copy means reading its whole body from the backend, so a complete pass re-reads the entire dataset and that egress is metered on most providers. Pick the period first, monthly being a normal operating point for bit rot, then derive the batch from the fleet size.

Two footguns are worth knowing. The scrubber runs on a tick and never at startup, so an interval longer than the process lifetime means it never runs at all while the dashboard still reports integrity as enabled. And `s3o_integrity_oldest_unverified_seconds` is the figure that tells you whether the sweep is keeping up: it should settle around the period implied by these two settings, and a steadily climbing value means the batch is too small or the interval too long.

**Usage limits bound the sweep.** A backend that has spent its configured egress or API allowance is excluded from the batch entirely, and the copies it holds are reported as `deferred` rather than checked. The coverage figures deliberately do not improve for them: a sweep that could not afford to read a backend has not verified it, and `s3o_integrity_oldest_unverified_seconds` keeps climbing so the gap stays visible instead of resolving into an apparently clean pass. Deferred work is also counted by `s3o_usage_limit_rejections_total{operation="scrub"}`.

**Integrity is hot-reloadable** — changes take effect on SIGHUP without a restart.

### cache

Optional in-memory LRU cache for full GET responses. Reduces backend API calls and egress by serving repeated reads from memory. Per-instance only — not shared across instances.

```yaml
cache:
  enabled: true
  max_size: "256MB"            # total cache capacity (default: 256MB)
  max_object_size: "10MB"      # largest cacheable object (default: 10MB)
  ttl: "5m"                    # per-entry time-to-live (default: 5m)
```

- `max_size` — total memory the cache may consume. Size this based on available container memory after accounting for the Go heap, connection pools, and streaming buffers. A good starting point is 10-25% of the container's memory allocation.
- `max_object_size` — objects larger than this are never admitted to the cache. Prevents a single large object from evicting many smaller frequently-accessed objects. Set this below the typical "hot" object size in your workload.
- `ttl` — maximum time an entry stays cached before automatic expiry. In multi-instance deployments, this bounds how stale a cached object can be when writes happen on another instance. Lower values reduce staleness at the cost of more backend requests.

Cache entries are automatically invalidated on PutObject, DeleteObject, CopyObject, DeleteObjects, and CompleteMultipartUpload. Range requests bypass the cache on miss but are served from cache on hit.

**When to enable:**
- Read-heavy workloads where the same objects are fetched repeatedly (thumbnails, config files, assets)
- Backends with per-request API charges or egress costs
- High-latency backends where caching improves P99 latency

**When to skip:**
- Write-heavy workloads with few repeated reads
- Objects are too large to fit meaningfully in memory
- Single-instance with very low read traffic

The cache is **not hot-reloadable** — changing cache settings requires a restart. When encryption is enabled, the cache stores post-decryption plaintext.

**Metrics:**

| Metric | Labels | Description |
|--------|--------|-------------|
| `s3o_integrity_checks_total` | `operation` | Hash verifications performed (read, scrub) |
| `s3o_integrity_errors_total` | `operation` | Hash mismatches detected (read, scrub) |

When enabled, the dashboard is served at `{path}/` on the same port as the S3 API.

All dashboard responses include security headers (`X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: strict-origin-when-cross-origin`, `Content-Security-Policy`). The dashboard requires authentication via the configured `admin_key`/`admin_secret` — unauthenticated requests are redirected to the login page (HTML) or receive `401` (API).


## Configuration hot-reload


The orchestrator supports hot-reloading a subset of configuration by sending `SIGHUP` to the running process. This lets you update credentials, quotas, rate limits, and other operational settings without restarting the service or dropping client connections.

```bash
kill -HUP $(pidof s3-orchestrator)
```

### Reloadable vs non-reloadable settings

| Setting | Reloadable | Notes |
|---------|:----------:|-------|
| `buckets` (credentials, limits) | Yes | Credentials and `max_multipart_uploads` take effect immediately |
| `rate_limit` | Yes | New visitors get updated rates; existing per-IP limiters expire naturally |
| `backends[].quota_bytes` | Yes | Synced to database on reload |
| `backends[].api_request_limit` | Yes | |
| `backends[].egress_byte_limit` | Yes | |
| `backends[].ingress_byte_limit` | Yes | |
| `backends[].http` (pool sizes, `force_http2`) | No | The transport is built when the backend client is constructed; a reload reports the change and keeps the running one |
| `rebalance` | Yes | Strategy, interval, threshold, concurrency, enabled/disabled |
| `replication` | Yes | Factor, worker interval, batch size |
| `usage_flush` | Yes | Interval, adaptive enabled/threshold/fast interval |
| `lifecycle` | Yes | Rules (prefix, expiration_days) |
| `integrity` | Yes | Enabled, verify_on_read, scrubber interval/batch size |
| `server.listen_addr` | No | Requires restart |
| `server.max_concurrent_requests` | No | Requires restart |
| `server.max_concurrent_reads` | No | Requires restart |
| `server.max_concurrent_writes` | No | Requires restart |
| `server.load_shed_threshold` | No | Requires restart |
| `server.admission_wait` | No | Requires restart |
| `server.max_header_bytes` | No | Requires restart |
| `server.max_header_value_count` | No | Requires restart |
| `server` timeouts | No | `read_header_timeout`, `read_timeout`, `write_timeout`, `idle_timeout`, `shutdown_delay` |
| `server.tls` | No | Requires restart |
| `database` | No | Requires restart |
| `telemetry` | No | Requires restart |
| `circuit_breaker` | No | Requires restart |
| `backend_circuit_breaker` | No | Requires restart |
| `ui` | No | Requires restart |
| `encryption` | No | Requires restart |
| `compression` | No | Requires restart |
| `cache` | No | Requires restart |
| `redis` | No | Requires restart |
| `routing_strategy` | No | Requires restart |
| `reconcile` | No | Requires restart |
| `backends` (structural: endpoint, credentials, count) | No | Requires restart |

On a successful reload, the orchestrator logs each reloaded section:

```
{"level":"INFO","msg":"SIGHUP received, reloading configuration","path":"config.yaml"}
{"level":"INFO","msg":"Reloaded bucket credentials","buckets":2}
{"level":"INFO","msg":"Reloaded rate limits","requests_per_sec":100,"burst":200}
{"level":"INFO","msg":"Reloaded backend quota limits"}
{"level":"INFO","msg":"Reloaded backend usage limits"}
{"level":"INFO","msg":"Reloaded rebalance/replication/usage-flush config"}
{"level":"INFO","msg":"Configuration reload complete"}
```

If the new config file is invalid, the orchestrator keeps the current configuration and logs the error:

```
{"level":"ERROR","msg":"Config reload failed, keeping current config","error":"invalid config: ..."}
```

Non-reloadable field changes are logged as warnings but do not prevent the reload of other settings:

```
{"level":"WARN","msg":"Config field changed but requires restart to take effect","field":"server.listen_addr"}
```
