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
# s3-orchestrator v0.41.7 go1.26.0 linux/amd64
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

### v0.46.x (current)

**SigV4 verifier honours wire-form path encoding (v0.46.7)**

The SigV4 canonical-request builder previously fed `r.URL.Path` (Go's
*decoded* URL path) into the path canonicaliser, then re-percent-encoded
each segment. AWS SDKs sign against the wire form (`EscapedPath()` /
`RawPath`). For any key whose URL-encoded shape was not byte-identical
to the decoded form  -  most importantly keys containing `%2F`  -  the
verifier's canonical request diverged from the client's, the signature
mismatched, and the request was rejected with 403 even when signed
correctly.

The fix switches both the header-based and presigned canonical-request
paths to use the wire form (`RawPath` when set, `Path` as fallback) and
rewrites the path encoder as a passthrough that preserves `%XX`
sequences verbatim and only encodes raw bytes the wire form did not
already encode.

**Operator action items after upgrade:**

- Clients that previously hit 403 SignatureDoesNotMatch on keys
  containing `%2F`, raw `%`, `+`, or other characters that Go's URL
  parser normalises will start succeeding. Watch for a one-time uptick
  in successful PUT/GET/DELETE on keys that the orchestrator was
  previously rejecting.
- No configuration change. The fix is purely a verifier correctness
  improvement.

**Multipart upload bucket isolation (v0.46.6)**

`UploadPart`, `CompleteMultipartUpload`, `AbortMultipartUpload`, and
`ListParts` previously accepted a bare `uploadId` from the query string
without checking that the upload belonged to the bucket on the request
URL. An authenticated caller for any bucket could manipulate in-flight
multipart uploads owned by another bucket: write parts into them, abort
them, or complete them under their own bucket's URL — silent cross-tenant
data corruption with no detection signal.

The fix adds bucket and key parameters to the manager-layer methods that
take `uploadID`. Each call fetches the upload's stored `ObjectKey` and
rejects with `404 NoSuchUpload` when the URL's bucket/key pair does not
match. Internal background paths (stale-upload cleanup, drain abort)
operate on resolved upload rows directly and do not need this guard.

**Operator action items after upgrade:**

- No configuration change. The fix is purely additive validation; clients
  that were already using their own buckets see no behaviour change.
- Audit logs continue to emit the same `storage.UploadPart`,
  `storage.CompleteMultipartUpload`, and `storage.AbortMultipartUpload`
  events. A new 404 NoSuchUpload response from any of these endpoints in
  production traffic that was previously succeeding indicates a client
  was relying on the broken cross-bucket behaviour and should be
  investigated.

**Cleanup queue per-row claim pattern eliminates double-processing (v0.46.5)**

`cleanup_queue` rows could be picked up by two worker goroutines (across
instances or across reconnects within an instance) because the original
`SELECT ... LIMIT N` had no row-level reservation. A connection death that
released the cleanup queue advisory lock mid-tick let a second instance
refetch rows the first instance was still processing; the duplicate run
double-decremented `orphan_bytes`, double-billed the backend `DELETE`,
and made backend routing trust an under-counted orphan total.

The fix adds two columns to `cleanup_queue` (`claimed_at TIMESTAMPTZ`,
`claimed_by TEXT`) and replaces the worker's `GetPendingCleanups` call
with `ClaimPendingCleanups`, which uses `UPDATE ... WHERE id IN (SELECT
... FOR UPDATE SKIP LOCKED)` so two concurrent claim transactions return
disjoint row sets. `CompleteCleanupItem` is now a single CTE that deletes
the row and decrements `orphan_bytes` atomically, so a worker crash
between the two operations cannot leave the counter inconsistent. A claim
older than the configured grace period is reclaimable so a worker that
died mid-process does not leave the row stuck.

**Database migration:** `00011_cleanup_queue_claim` runs automatically on
startup. The migration uses `+goose NO TRANSACTION` plus
`CREATE INDEX CONCURRENTLY` so applying it against a populated table does
not require a write outage. `ExpectedSchemaVersion` is bumped 10 → 11.

**New configuration field:**

```yaml
cleanup_queue:
  claim_grace_period: 5m   # reclaim stale per-row claims older than this
```

The default is 5 minutes; existing configs continue to work without the
field. Hot-reloadable.

**Operator action items after upgrade:**

- Add an alert on `rate(s3o_cleanup_queue_stale_claims_recovered_total[5m]) > 0`
  -- a non-zero rate means a worker died mid-process or the grace period
  is shorter than realistic worst-case row processing time.
- Watch `cleanup_queue.claim_recovered` audit events for the same signal
  with per-row context (cleanup_id, backend, key, reclaimed_by).

**Encryption stream readers no longer silently truncate on transport errors (v0.46.4)**

Both `encryptReader.Read` and `decryptReader.Read` previously translated any
non-nil error from `io.ReadFull` into a clean `io.EOF` when the source
returned zero bytes. The branch fired indistinguishably for real
end-of-stream and for transient transport failures (network reset, context
cancellation, backend timeout). Consumers -- the replicator, the scrubber,
and the GET proxy path -- saw a clean truncated stream and treated it as
the whole object.

The readers now distinguish `errors.Is(err, io.EOF)` from arbitrary errors
and propagate non-EOF failures wrapped with operation context. Streaming
errors that surface mid-Read also increment
`s3o_encryption_errors_total{op,error_type="stream_failed"}` so operators
have an alertable signal.

**Operator action items after upgrade:**

- Add an alert on `rate(s3o_encryption_errors_total{error_type="stream_failed"}[5m])`
  once the new label starts emitting.
- Run a scrub pass on encrypted objects to surface any pre-existing
  truncated replicas; the read-time integrity check (`verify_on_read`)
  will flag them.

**Streaming SigV4 chunk validation (v0.46.3)**

The SigV4 verifier previously accepted the streaming-payload sentinels
(`STREAMING-AWS4-HMAC-SHA256-PAYLOAD`, `STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER`,
`STREAMING-UNSIGNED-PAYLOAD-TRAILER`) as the canonical-request payload
hash without validating the per-chunk signatures or stripping the
`aws-chunked` framing. A request whose seed signature was valid could
ship arbitrary body bytes; the framing landed in the stored object. The
orchestrator now wraps the request body in a chunk-validating reader
that verifies each chunk-signature in the chain (or the trailer
signature for the unsigned-trailer variant), enforces
`x-amz-decoded-content-length`, and rejects malformed framing before
any byte reaches storage.

**Behavioural changes:**

- Streaming-payload PUTs now have their bodies validated on the wire.
  Conforming clients (`aws-cli`, `aws-sdk-go-v2`, `boto3`, `minio-go`,
  etc.) work without any client-side change.
- A request signed with a streaming sentinel but carrying mismatched or
  bogus chunk signatures is rejected with `403 SignatureDoesNotMatch`.
- A request whose body is shorter or longer than
  `x-amz-decoded-content-length` is rejected with `400 IncompleteBody`.
- Malformed chunk framing (bare LF, missing CRLF, malformed hex size,
  missing `chunk-signature=` extension on a signed variant, missing
  `x-amz-trailer-signature` on a trailer variant) returns
  `400 InvalidRequest`.

**New metrics:**

- `s3o_auth_streaming_requests_total{variant}` -- count of streaming
  requests received, labelled `signed`, `signed_trailer`, or
  `unsigned_trailer`.
- `s3o_auth_streaming_rejections_total{reason}` -- count of streaming
  requests rejected mid-stream, labelled by reason
  (`chunk_signature_mismatch`, `trailer_signature_mismatch`,
  `chunk_malformed`, `chunk_too_large`, `decoded_length_mismatch`,
  `trailer_malformed`).

**Operator action items after upgrade:**

- Run the new diagnostic test against your cluster to detect any
  pre-existing on-disk corruption from clients that streamed before
  this release. The test is gated by `//go:build diag` and reads from
  the orchestrator's S3 API:

  ```bash
  DIAG_S3_ENDPOINT=https://s3.example.com \
  DIAG_S3_ACCESS_KEY=... DIAG_S3_SECRET_KEY=... \
  go test -tags=diag -run TestScanChunkedFraming -v -timeout=30m \
      ./internal/integration/chunkframing/...
  ```

  The test prints one `t.Errorf` line per object whose stored body
  begins with `aws-chunked` framing and exits non-zero if any are
  found.

- Set up an alert on any non-zero rate of
  `s3o_auth_streaming_rejections_total` -- every increment is either a
  legitimate client misconfiguration or a tampered request.

### v0.44.x

**Cleanup queue dead-letter for unrecoverable orphans**

Cleanup queue rows that exhausted their retry budget previously stayed pinned in `cleanup_queue` with `attempts >= 10`, invisible to the worker (filtered out by the partial index) and surfaced only by a single counter increment. They are now graduated to a new `cleanup_dlq` table by `core.MoveCleanupToDLQ` so an operator can find them, retry them manually, or write each one off deliberately.

**Database migration:**

- `00009_cleanup_dlq.sql` — adds the `cleanup_dlq` table (auto-applied on startup). The columns mirror `cleanup_queue` plus `original_id`, `first_enqueued_at`, and `moved_at` so each DLQ row carries enough context to investigate the orphan.

**Behavioral changes:**

- **Exhaustion path** — the cleanup worker now calls `MoveCleanupToDLQ(id, last_error)` instead of `RetryCleanupItem(id, 0, ...)` when `attempts` reaches 10. The move is a single transaction (read queue row → insert DLQ row → delete queue row) so the row is never duplicated or lost.
- **Quota accounting unchanged** — `orphan_bytes` is intentionally NOT decremented when a row is moved to the DLQ. The backend object is still on disk; the bytes really are still occupying the backend's quota. Reclaim happens only when an operator confirms the object is gone (e.g. via the reconciler) and writes off the row deliberately.

**New metrics:**

- `s3o_cleanup_dlq_depth` (gauge) — current count of unrecoverable orphans waiting in the DLQ.
- `s3o_cleanup_dlq_enqueued_total{backend}` (counter) — rate of graduations per backend; one backend dominating means that backend's delete path is broken.

**New audit event:**

- `cleanup_queue.exhausted_to_dlq` — emitted with the row's key, backend, attempts, size_bytes, and final last_error each time a queue row is graduated.

**Operator action items after upgrade:**

- Set up an alert on `s3o_cleanup_dlq_depth > 0` so unrecoverable orphans surface promptly.
- Use the SQL recipes in [admin-guide.md#cleanup-queue](../admin-guide/#cleanup-queue) and [disaster-recovery.md#cleanup-queue-recovery](../disaster-recovery/#cleanup-queue-recovery) to inspect and resolve DLQ entries.

### v0.41.x

**New feature: Integrity verification**

SHA-256 content hashing for object integrity verification. When enabled, objects are checksummed on write and optionally verified on read and by a background scrubber.

**Database migration:**

- Migration `00005_add_content_hash` adds a nullable `content_hash TEXT` column to `object_locations`. Applied automatically on startup.

**New config section:**

```yaml
integrity:
  enabled: false                     # Enable integrity verification
  verify_on_read: false              # Hash-check GET responses as they stream
  verify_on_replicate: true          # Verify hash when creating replicas (default when enabled)
  scrubber_interval: "6h"            # Background verification interval (0 = disabled)
  scrubber_batch_size: 100           # Objects per scrub cycle
```

All fields are optional and default to disabled. This is a non-breaking change — existing configs work without modification.

**New admin commands:**

- `admin scrub [-batch-size N]` — trigger an on-demand integrity scrub cycle.
- `admin backfill-checksums [-batch-size N]` — compute and store hashes for objects written before integrity was enabled.

**New metrics:**

- `s3o_integrity_checks_total{operation}` — hash verifications performed (read, scrub).
- `s3o_integrity_errors_total{operation}` — hash mismatches detected (read, scrub).

**Behavioral notes:**

- Integrity config is hot-reloadable via SIGHUP.
- The scrubber reads objects from backends, which counts against usage quota (API calls + egress).
- Encrypted objects are decrypted before hashing — the hash is always computed on plaintext.

### v0.19.x

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
- `remove-backend --purge` now requires `--confirm` flag. Without it, `--purge` is a dry-run that shows what would be destroyed. API requires two-phase confirmation with a signed token (60s TTL).
- UI API POST requests now require a `X-CSRF-Token` header matching the `s3orch_csrf` cookie (double-submit cookie pattern). GET requests are unaffected.
- `/health` and `/health/ready` responses no longer include the `instance` field (hostname).
- Prometheus metrics can be served on a separate listener via `telemetry.metrics.listen`.

**New config fields:**

- `buckets[].max_multipart_uploads` — optional limit on active multipart uploads per bucket (default: 0, unlimited). Returns `503 SlowDown` when exceeded.
- `telemetry.metrics.listen` — optional separate address for the metrics endpoint (e.g., `127.0.0.1:9091`).

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
