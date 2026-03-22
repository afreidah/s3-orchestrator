This document covers upgrading between versions of the S3 Orchestrator, including database migrations, configuration changes, and breaking changes.

## How Upgrades Work

### Database Migrations

Database schema changes are handled automatically. The orchestrator embeds [goose](https://github.com/pressly/goose) migrations in the binary and applies any pending migrations on startup. No manual migration step is required.

```
{"level":"INFO","msg":"Database migrations applied"}
```

If a migration fails, the orchestrator logs the error and exits without starting. Fix the underlying issue and restart.

### Configuration

Configuration changes are **not** applied automatically. When a new version adds or changes config fields:

- New optional fields use sensible defaults, so existing configs continue to work.
- Removed or renamed fields cause a validation error on startup with a clear message.
- The `validate` subcommand lets you check a config file against a new binary before deploying:

  ```bash
  s3-orchestrator validate -config config.yaml
  ```

### Checking Your Version

```bash
s3-orchestrator version
# s3-orchestrator v0.8.23 go1.26.0 linux/amd64
```

## Compatibility Matrix

| Component | Tested Versions | Compatible | Notes |
|-----------|----------------|------------|-------|
| PostgreSQL | 16, 18 | 10+ (pgx driver) | Required. Connection pooling via pgxpool. |
| Redis | 7, 8 | 6+ (go-redis driver) | Optional. Shared usage counters for multi-instance deployments. |
| Go | 1.26 | 1.26+ | For building from source. Version set in `go.mod`. |
| S3 backends | MinIO, OCI, R2 | Any S3-compatible API | GCS requires `disable_checksum`, `unsigned_payload`, `strip_sdk_headers`. |
| Container runtime | Docker | Docker, containerd, Podman | Multi-arch images (amd64, arm64) published to ghcr.io. |
| Orchestrators | Nomad, Kubernetes | Nomad, Kubernetes, systemd | Deploy manifests and demo scripts provided. |

## Upgrade Checklist

Before upgrading to a new version:

- [ ] Back up the PostgreSQL database (`pg_dump`)
- [ ] Review breaking changes for the target version below
- [ ] Validate config against the new binary: `s3-orchestrator validate -config config.yaml`
- [ ] Update Grafana dashboards if metric names changed (check version notes)
- [ ] Deploy to staging and verify `/health/ready` returns `{"status":"ready"}`
- [ ] Test a PUT + GET round-trip in staging
- [ ] Deploy to production with rolling update (readiness probe gates traffic)

To roll back: restore the database backup and deploy the previous binary version. Schema migrations are forward-only — downgrade requires a database restore.

## Version History

### v0.19.x (current)

**Breaking changes:**

- `encryption.NewEncryptor` now returns `(*Encryptor, error)` instead of `*Encryptor`. Callers must handle the error.
- `LoginThrottle.IsLockedOut`, `RecordFailure`, and `RecordSuccess` accept a resolved client IP string instead of a raw `remoteAddr`. Callers are responsible for IP extraction via `ExtractClientIP`.

**Config validation:**

- `encryption.master_key_file` must exist and be exactly 32 bytes at startup. Previously validated only at first use.
- Invalid worker pool concurrency (≤ 0) logs a warning when clamped to 1.

**Metrics:**

- `s3o_rebalance_pending` (gauge) — objects planned for rebalance in the current cycle.
- `s3o_encryption_unknown_key_id_total` (counter) — decryption attempts with an unrecognized keyID.

**Behavioral changes:**

- `Close()` is idempotent on `RedisCounterBackend`, `RateLimiter`, and `LoginThrottle`.
- Parallel broadcast reads cancel losing goroutine contexts on first success.
- Backend drain queries only the target backend's multipart uploads (`GetMultipartUploadsByBackend`).
- UI API error responses return `Content-Type: application/json`.
- UI login evaluates `checkSecret` unconditionally to prevent timing side-channel on access key validity.
- Admin token check no longer short-circuits on empty token.

### v0.14.x

**New configuration fields:**

- `server.max_concurrent_reads` -- separate concurrency limit for read operations (GET, HEAD). Default: 0 (uses global limit).
- `server.max_concurrent_writes` -- separate concurrency limit for write operations (PUT, POST, DELETE). Default: 0 (uses global limit).
- `server.load_shed_threshold` -- active load shedding threshold as a fraction of pool capacity (0.0--1.0). When in-flight requests exceed this ratio, new requests are probabilistically rejected with probability ramping linearly to 100% at full capacity. Default: 0 (disabled).
- `server.admission_wait` -- brief wait duration before rejecting when the admission semaphore is full (e.g. `50ms`, `100ms`). Smooths micro-bursts without adding latency during sustained overload. Default: 0 (instant rejection).

**Behavioral changes:**

- **Retry-After headers** -- 503 (admission control) and 429 (rate limit) responses now include a `Retry-After: 1` header. AWS S3 SDKs and well-behaved HTTP clients use this for backoff timing instead of retrying immediately.
- **Early upload rejection** -- PUT requests are pre-checked for backend capacity before the request body is read. When clients send `Expect: 100-continue`, uploads to full backends are rejected without transmitting the body, saving bandwidth.
- **Separate read/write admission pools** -- when `max_concurrent_reads` and `max_concurrent_writes` are both set, reads and writes get independent concurrency limits. A burst of large uploads no longer starves GETs and HEADs.
- **Active load shedding** -- when `load_shed_threshold` is set, requests are probabilistically rejected before the hard admission limit. This provides smooth degradation instead of a cliff at the concurrency limit.
- **Admission queue timeout** -- when `admission_wait` is set, requests briefly wait for a slot before being rejected, smoothing short traffic spikes.

**New metrics:**

- `s3o_load_shed_total` (counter) -- requests probabilistically shed before the hard admission limit
- `s3o_early_rejections_total` (counter) -- uploads rejected before body transmission due to no backend capacity

### v0.13.x

**Performance improvements:**

- **Dedicated HTTP transport per backend** -- each S3 backend now gets its own `http.Transport` with tuned connection pool settings (100 max idle, 90s idle timeout, 30s keepalive, 10s dial/TLS timeouts). Improves throughput by reducing connection setup latency and provides per-backend resource isolation.
- **DNS freshness via idle connection recycling** -- the 90-second `IdleConnTimeout` forces fresh DNS resolution on reconnection, allowing the orchestrator to follow backend endpoint changes without restarts.
- **Shared buffer pool for streaming** -- a `sync.Pool` of reusable 32 KB buffers replaces per-call `io.Copy` allocations at all streaming sites (GET proxy, PUT body buffering, CopyObject, multipart assembly, UI downloads), reducing GC pressure under high concurrency.

### v0.12.x

**Database migrations:**

- `00004_add_orphan_bytes.sql` -- adds `orphan_bytes` column to `backend_quotas` and `size_bytes` column to `cleanup_queue` (auto-applied on startup)

**New configuration fields:**

- `replication.concurrency` -- parallel object replications per cycle (default: 5)
- `cleanup_queue.concurrency` -- parallel cleanup deletions per worker tick (default: 10)

**Behavioral changes:**

- **Worker pool parallelism** — the cleanup worker, replicator, single-key DeleteObject, batch DeleteObjects, and rebalancer now use a shared bounded-concurrency worker pool. The cleanup worker and replicator concurrency are configurable; the rebalancer retains its existing `rebalance.concurrency` field.
- **Orphan bytes tracking** — the cleanup queue now tracks the size of each enqueued item. On enqueue, the backend's `orphan_bytes` counter is incremented; on successful cleanup, it is decremented. All capacity checks (write routing, replication target selection, spread utilization ratio) subtract `orphan_bytes` from available space to prevent quota overcommitment during backend outages.
- **Exhausted cleanup items preserved** — items that exceed 10 retry attempts remain in the queue with `orphan_bytes` still reserved, rather than being removed. This prevents the write path from overcommitting storage. Operators must manually resolve these items.
- **Overwrite displaced copies** — when a PutObject overwrites an existing key, stale copies on other backends are now enqueued for cleanup with their size tracked, rather than being silently abandoned if the immediate delete fails.

**New metrics:**

- `s3o_quota_orphan_bytes` (gauge, `backend` label) — bytes reserved by pending cleanup items per backend

### v0.11.x

**New configuration fields:**

- `rate_limit.cleanup_interval` -- stale entry eviction interval (default: 1m)
- `rate_limit.cleanup_max_age` -- entries not seen within this window are evicted (default: 5m)
- `redis` section -- optional shared usage counters via Redis for multi-instance deployments
  - `redis.address` -- Redis host:port (required when section is present)
  - `redis.password` -- AUTH password (optional)
  - `redis.db` -- Redis database number (default: 0)
  - `redis.tls` -- enable TLS (default: false)
  - `redis.key_prefix` -- key namespace (default: "s3orch")
  - `redis.failure_threshold` -- circuit breaker threshold (default: 3)
  - `redis.open_timeout` -- circuit breaker probe delay (default: 15s)
- `backends[].disable_checksum` -- disable AWS SDK default checksums (default: false). Required for Google Cloud Storage HMAC interoperability, where the SDK's streaming CRC64NVME checksums cause `SignatureDoesNotMatch` errors.
- `backends[].strip_sdk_headers` -- strip AWS SDK v2 headers (`amz-sdk-invocation-id`, `amz-sdk-request`, `accept-encoding`) and the `x-id` query parameter before request signing (default: false). Required for Google Cloud Storage, where the SDK-added headers cause `SignatureDoesNotMatch` because GCS does not include them in signature verification.

**Behavioral changes:**

- `unsigned_payload` on HTTP backends is no longer force-disabled when explicitly set to `true`. Previously, the orchestrator always forced signed (buffered) payloads over HTTP regardless of config. Now an explicit `unsigned_payload: true` is respected, which is required for large uploads to HTTP backends (MinIO, etc.) to avoid buffering the entire object in memory. HTTPS backends continue to default to unsigned payload automatically.

**New features:**

- `x-amz-meta-*` user metadata passthrough on PutObject, GetObject, HeadObject, CopyObject, and multipart uploads
- `govulncheck` CI job for Go dependency vulnerability scanning
- Optional Redis shared counters for multi-instance usage tracking with circuit breaker fallback to local counters
- Dashboard file download — download individual objects directly from the file tree in the admin UI

**New dependencies:**

- `github.com/redis/go-redis/v9`

**Database migrations:**

- `00002_multipart_metadata.sql` -- adds `metadata` JSONB column to `multipart_uploads` table (auto-applied on startup)

### v0.8.x

**New configuration fields:**

- `server.log_level` -- runtime log level (debug, info, warn, error). Default: `info`. Reloadable via SIGHUP.

**New features:**

- Admin API and CLI (`s3-orchestrator admin`) for operational tasks
- Top-level `help` subcommand listing all available commands

### v0.7.x

**New configuration fields:**

- `lifecycle.rules[]` -- object expiration rules with prefix matching and configurable retention
- `server.shutdown_delay` -- pre-stop delay for load balancer deregistration (default: 0)

### v0.6.x

**New configuration fields:**

- `backends[].ingress_byte_limit` -- monthly ingress byte limit per backend
- `usage_flush.adaptive_enabled`, `usage_flush.adaptive_threshold`, `usage_flush.fast_interval` -- adaptive usage flushing near limits
- `circuit_breaker.parallel_broadcast` -- fan-out reads during degraded mode

### v0.5.x

**New configuration fields:**

- `backends[].api_request_limit`, `backends[].egress_byte_limit` -- monthly usage limits
- `usage_flush` section -- periodic usage counter flush settings
- `server.read_header_timeout`, `server.read_timeout`, `server.write_timeout`, `server.idle_timeout` -- HTTP server timeouts
- `server.tls` section -- TLS and mTLS support

### v0.4.x

**New configuration fields:**

- `ui` section -- built-in web dashboard
- `rate_limit` section -- per-IP rate limiting
- `circuit_breaker.cache_ttl` -- key-to-backend cache during degraded mode

### v0.3.x

**New configuration fields:**

- `replication` section -- cross-backend object replication
- `rebalance.concurrency` -- parallel move operations

### v0.2.x

**New configuration fields:**

- `rebalance` section -- periodic backend rebalancing
- `buckets` section -- multi-bucket support with per-bucket credentials
- `routing_strategy` -- pack or spread write routing

### v0.1.x

Initial release with core functionality:

- Single-backend S3 proxy with PostgreSQL metadata
- Quota-based write routing across multiple backends
- Basic SigV4 authentication

## Rollback Considerations

### Database

Goose migrations support `-- +goose Down` sections for rollback. However, rolling back database migrations is generally not recommended in production because:

- Down migrations may drop columns or tables that the newer version populated with data.
- The older binary may not understand schema changes made by the newer version's startup logic.

**Recommended approach:** take a database backup before upgrading and restore it if you need to roll back.

```bash
# Before upgrade
pg_dump -h localhost -U s3proxy s3proxy > backup.sql

# If rollback needed
psql -h localhost -U s3proxy s3proxy < backup.sql
```

### Configuration

Keep the previous config file when upgrading. New versions only add fields with defaults -- they don't change the meaning of existing fields.

### Binary

The orchestrator is a single static binary. Roll back by deploying the previous version:

```bash
# Debian package
apt install s3-orchestrator=0.7.0

# Docker
docker pull ghcr.io/afreidah/s3-orchestrator:v0.7.0

# Binary
# Replace with the previous version's binary and restart
```

## Breaking Changes Policy

Starting with v1.0.0, the project will follow semantic versioning:

- **Patch** (v1.0.x): bug fixes, no config or API changes
- **Minor** (v1.x.0): new features, new optional config fields, backward-compatible
- **Major** (vX.0.0): breaking config changes, removed fields, incompatible API changes

Pre-v1.0.0 releases may include breaking changes in minor versions. Always check this document before upgrading.
