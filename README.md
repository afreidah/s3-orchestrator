<p align="center">
  <img src="docs/images/logo.png" alt="s3-orchestrator" width="350">
</p>

# s3-orchestrator

[![CI](https://github.com/afreidah/s3-orchestrator/actions/workflows/ci.yml/badge.svg)](https://github.com/afreidah/s3-orchestrator/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/afreidah/s3-orchestrator/branch/main/graph/badge.svg)](https://codecov.io/gh/afreidah/s3-orchestrator)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

<p align="center">
  <strong><a href="https://s3-orchestrator.munchbox.cc">Project Website</a></strong> · <strong><a href="https://s3-orchestrator.munchbox.cc/docs/">Documentation</a></strong> · <strong><a href="https://s3-orchestrator.munchbox.cc/guides/maximizing-free-tiers/">Maximizing Free-Tier Storage</a></strong>
</p>

An S3-compatible orchestrator that combines multiple storage backends into a single unified endpoint. Add as many S3-compatible backends as you want — OCI Object Storage, Backblaze B2, AWS S3, MinIO, whatever — and the orchestrator presents them to clients as one or more virtual buckets. Per-backend quota enforcement lets you cap each backend at exactly the byte limit you choose, so you can stack multiple free-tier or cost-limited allocations from different providers into a single, larger storage target for backups, media, etc without worrying about surprise bills.

Multiple virtual buckets let different applications share the same orchestrator with isolated file namespaces and independent credentials. Each bucket's objects are stored with an internal key prefix (`{bucket}/{key}`), so bucket isolation requires zero changes to the storage layer or database schema.

Built-in cross-backend replication also makes this an easy way to keep your data in multiple clouds without touching your application. Point your app at the proxy, set a replication factor, and every object automatically lands in two or more providers — instant multi-cloud redundancy with zero client-side changes.

Objects are routed to backends based on the configured `routing_strategy`: **pack** (default) fills backends in config order, while **spread** places each write on the least-utilized backend by ratio. Metadata and quota tracking live in PostgreSQL; the backends only see standard S3 API calls. The orchestrator is fully S3-compatible and works with any standard S3 client.

## Getting Started

**Prerequisites:** Go 1.26+, Docker, Make.

```bash
git clone https://github.com/afreidah/s3-orchestrator.git
cd s3-orchestrator
make run
```

This starts three MinIO backends via Docker Compose (the orchestrator uses embedded SQLite by default, so no external database is needed), then launches the orchestrator on `localhost:9000`. Test it:

```bash
aws --endpoint-url http://localhost:9000 s3 cp /etc/hostname s3://photos/test.txt
aws --endpoint-url http://localhost:9000 s3 ls s3://photos/
```

Default credentials: access key `photoskey`, secret `photossecret`. Web dashboard at [localhost:9000/ui/](http://localhost:9000/ui/) (login: `admin` / `admin`).

See the [Quickstart](docs/quickstart.md) for full details, credentials for all buckets, and troubleshooting.

**Other ways to install:**
- Docker: `docker pull ghcr.io/afreidah/s3-orchestrator:<version>`
- Debian: download `.deb` from [GitHub Releases](https://github.com/afreidah/s3-orchestrator/releases)
- Binary: download from [GitHub Releases](https://github.com/afreidah/s3-orchestrator/releases)

> **Database:** SQLite is embedded by default — no external database needed for single-instance use. For multi-instance deployments, configure `database.driver: postgres` with PostgreSQL 14+. Run `s3-orchestrator init` to generate a config file interactively.

**Verify artifact signatures:**

Container images and release checksums are signed with [cosign](https://github.com/sigstore/cosign) (keyless / Sigstore):

```bash
# Verify a container image
cosign verify ghcr.io/afreidah/s3-orchestrator:<version> \
  --certificate-identity-regexp='github\.com/afreidah/s3-orchestrator' \
  --certificate-oidc-issuer='https://token.actions.githubusercontent.com'

# Verify release checksums
cosign verify-blob checksums.txt \
  --signature checksums.txt.sig \
  --certificate checksums.txt.pem \
  --certificate-identity-regexp='github\.com/afreidah/s3-orchestrator' \
  --certificate-oidc-issuer='https://token.actions.githubusercontent.com'
```

**Operational CLI:** `s3-orchestrator admin --help` for rebalance, drain, encryption management, and backend sync.

## Table of Contents

- [Getting Started](#getting-started)
- [Architecture](#architecture)
- [S3 API Coverage](#s3-api-coverage)
- [Authentication & Multi-Bucket](#authentication--multi-bucket)
- [Degraded Mode (Database Circuit Breaker)](#degraded-mode-database-circuit-breaker)
- [Backend Circuit Breaker](#backend-circuit-breaker)
- [Write Routing](#write-routing)
- [Rebalancing](#rebalancing)
- [Replication](#replication)
- [Over-Replication Cleanup](#over-replication-cleanup)
- [Cleanup Queue](#cleanup-queue)
- [Lifecycle (Object Expiration)](#lifecycle-object-expiration)
- [Orphan Reconciliation](#orphan-reconciliation)
- [Encryption](#encryption)
- [Object Data Cache](#object-data-cache)
- [Rate Limiting](#rate-limiting)
- [Usage Limits](#usage-limits)
- [Configuration](#configuration)
- [Configuration Hot-Reload](#configuration-hot-reload)
- [Database](#database)
- [Telemetry](#telemetry)
- [Web UI](#web-ui)
- [Endpoints](#endpoints)
- [Background Tasks](#background-tasks)
- [Multi-Instance Deployment](#multi-instance-deployment)
- [CLI Subcommands](#cli-subcommands)
- [Development](#development)
- [Deployment](#deployment)
- [Project Structure](#project-structure)
- [Additional Documentation](#additional-documentation)

## Architecture

```
              S3 clients (aws cli, rclone, etc.)
                          |
                          v
                    +-----------+
                    | S3 Orch.  |  <-- SigV4 auth, rate limiting, quota routing
                    +-----------+
                     |         |
            +--------+         +------------------+------------------+
            v                  v                  v                  v
       PostgreSQL        OCI Object         Backblaze B2          AWS S3
       (metadata)       Storage (20 GB)       (10 GB)             (5 GB)
                              \                  |                  /
                               '------------ 35 GB total ---------'
```

- **PostgreSQL** stores object locations (`object_locations`), per-backend quota counters and orphan bytes tracking (`backend_quotas`), and multipart upload state (`multipart_uploads`, `multipart_parts`). Schema is applied automatically on startup via [goose](https://github.com/pressly/goose) versioned migrations embedded in the binary. All queries are generated by [sqlc](https://sqlc.dev/) from annotated SQL files and executed via [pgx/v5](https://github.com/jackc/pgx) connection pools.
- **Backends** are standard S3-compatible services accessed via AWS SDK v2, each with a dedicated tuned HTTP transport (connection pooling, idle timeout for DNS freshness). Streaming operations use a shared buffer pool to reduce GC pressure. Any provider that speaks the S3 API works -- OCI Object Storage, Backblaze B2, AWS S3, MinIO, Wasabi, etc.
- **Write routing** selects a backend for each new object based on the `routing_strategy`. In **pack** mode (default), objects go to the first backend in config order that has available quota — good for filling free-tier allocations sequentially. In **spread** mode, objects go to the backend with the lowest utilization ratio (`(bytes_used + orphan_bytes) / bytes_limit`) — good for distributing load evenly across backends. Quota is updated atomically in a transaction alongside the object location record. Set `quota_bytes: 0` (or omit it) to disable quota enforcement on a backend — useful when you don't need cost control and just want unified access or replication.
- **Usage limits** optionally cap monthly API requests, egress bytes, and ingress bytes per backend. When a backend exceeds a limit, writes overflow to other backends and reads fail over to replicas. Delete and abort operations always bypass limits. Limits are enforced using cached database totals (refreshed at the configured flush interval) plus unflushed counters (in Redis when configured, otherwise local in-memory atomics). Adaptive flushing automatically shortens the interval when any backend approaches a limit.

## S3 API Coverage

| Operation | Method | Path | Notes |
|-----------|--------|------|-------|
| ListBuckets | `GET` | `/` | Returns buckets the credential has access to |
| HeadBucket | `HEAD` | `/{bucket}` | Confirms bucket exists (200 if authorized) |
| GetBucketLocation | `GET` | `/{bucket}?location` | Returns empty `LocationConstraint` |
| PutObject | `PUT` | `/{bucket}/{key}` | Preserves `x-amz-meta-*` user metadata |
| GetObject | `GET` | `/{bucket}/{key}` | Supports `Range` header; returns `x-amz-meta-*` |
| HeadObject | `HEAD` | `/{bucket}/{key}` | Returns `x-amz-meta-*` user metadata |
| DeleteObject | `DELETE` | `/{bucket}/{key}` | Idempotent (404 from store treated as success) |
| DeleteObjects | `POST` | `/{bucket}?delete` | Batch delete up to 1000 keys per request |
| CopyObject | `PUT` | `/{bucket}/{key}` | Uses `X-Amz-Copy-Source` header (same-bucket only) |
| ListObjectsV1 | `GET` | `/{bucket}` | Original list API, uses `marker` pagination |
| ListObjectsV2 | `GET` | `/{bucket}?list-type=2` | Supports `delimiter` for virtual directories |
| ListMultipartUploads | `GET` | `/{bucket}?uploads` | Lists in-progress multipart uploads |
| CreateMultipartUpload | `POST` | `/{bucket}/{key}?uploads` | |
| UploadPart | `PUT` | `/{bucket}/{key}?partNumber=N&uploadId=X` | |
| CompleteMultipartUpload | `POST` | `/{bucket}/{key}?uploadId=X` | |
| AbortMultipartUpload | `DELETE` | `/{bucket}/{key}?uploadId=X` | |
| ListParts | `GET` | `/{bucket}/{key}?uploadId=X` | |

**Batch delete** (`DeleteObjects`) accepts an XML request body listing up to 1000 keys and returns per-key success/error results. Metadata removal is sequential (each key is its own DB transaction), while backend S3 deletes run concurrently with bounded parallelism for throughput. Failed backend deletes are enqueued to the cleanup queue for automatic retry. The response always returns HTTP 200, even when individual keys fail -- errors are reported per-key in the XML body. Quiet mode (`<Quiet>true</Quiet>`) suppresses the `<Deleted>` elements and only returns errors.

Each request must target a virtual bucket name that matches the credentials used to sign the request. Requests to a bucket the credentials aren't authorized for return `403 AccessDenied`.

Every response includes an `X-Amz-Request-Id` header with a unique request ID for tracing. Clients can supply their own ID via `X-Request-Id`; otherwise the orchestrator generates one. The same ID appears in audit logs and OpenTelemetry spans.

## Authentication & Multi-Bucket

Each virtual bucket has one or more credential sets. On every request, the orchestrator:

1. Extracts the access key from the SigV4 `Authorization` header, presigned URL query parameters, or token from `X-Proxy-Token`.
2. Looks up which bucket the credential belongs to.
3. Verifies the signature (SigV4 header or presigned query parameters) or token.
4. Validates the URL path bucket matches the authorized bucket.

Three auth methods are supported, checked in order:

1. **AWS SigV4** (recommended) - Standard AWS Signature Version 4 via the `Authorization` header. Compatible with `aws cli`, SDKs, and any S3 client. Signature verification is constant-time: unknown access keys still compute a full HMAC to prevent timing side-channel enumeration.
2. **Presigned URLs** - SigV4 query-parameter authentication (`X-Amz-Algorithm`, `X-Amz-Credential`, etc.) for time-limited, shareable URLs. Works with any AWS SDK presign client. Maximum expiry: 7 days. Uses the same bucket credentials as normal requests — no additional configuration required.
3. **Legacy token** - Simple `X-Proxy-Token` header for backward compatibility.

Multiple services can share a bucket by each having their own credentials that all map to the same bucket name. Access key IDs must be globally unique across all buckets.

Authentication is always required — every bucket must have at least one credential set.

For client usage examples (AWS CLI, rclone, boto3, Go SDK), see the [User Guide](docs/user-guide.md).
For deployment and operations, see the [Admin Guide](docs/admin-guide.md).

## Degraded Mode (Database Circuit Breaker)

A three-state circuit breaker wraps all database access:

```
closed (healthy) → open (DB down) → half-open (probing) → closed
```

When the database becomes unreachable (consecutive failures exceed `failure_threshold`), the orchestrator enters **degraded mode**:

- **Reads** broadcast to all backends in order (or in parallel if `parallel_broadcast` is enabled). A location cache (TTL configurable via `cache_ttl`) stores successful lookups to avoid repeated broadcasts for the same key.
- **Writes** (PUT, DELETE, COPY, multipart) return `503 ServiceUnavailable`.
- **Health endpoint** returns `degraded` instead of `ok`.

After `open_timeout` elapses, the circuit enters half-open state and sends a single probe request. If the database responds, the circuit closes and normal operation resumes automatically.

## Backend Circuit Breaker

Optional per-backend circuit breakers detect when individual S3 backends become unreachable (expired credentials, provider outage, network failure) and stop sending traffic to them until recovery is detected.

```
closed (healthy) → open (backend down) → half-open (probing) → closed
```

When a backend accumulates `failure_threshold` consecutive failures, the circuit opens:

- **Writes** skip the unhealthy backend and route to other backends with available quota.
- **Reads** fail over to replicas on healthy backends (requires `replication.factor >= 2`).
- **Replication** creates replacement copies on healthy backends after a sustained outage (see [Health-Aware Replication](#health-aware-replication)).
- All calls to the backend return `ErrBackendUnavailable` immediately — no timeout waiting.

After `open_timeout` elapses (plus randomized jitter of up to `open_timeout/4`), the next organic request to the backend is allowed through as a probe. If it succeeds, the circuit closes. If it fails, the circuit reopens for another timeout period. The jitter is recomputed on each open transition to prevent multiple backends from probing simultaneously after a shared failure event.

A background watchdog service checks all circuit breakers every minute for stale half-open probes. If a probe has been in flight longer than 2 minutes (e.g. the backend accepted the TCP connection but never responded), the watchdog resets the circuit to open so a new probe can be dispatched. This prevents circuits from getting permanently stuck half-open on low-traffic backends where no new request arrives to trigger the passive stale-probe detection.

Unlike the database circuit breaker, backend circuit breakers treat **all** errors as failures (no error filtering). This is a per-backend wrapper — each backend has its own independent circuit breaker state.

```yaml
backend_circuit_breaker:
  enabled: true
  failure_threshold: 5   # consecutive failures before opening (default: 5)
  open_timeout: "5m"     # delay before probing recovery (default: 5m)
```

Disabled by default. Requires a restart to enable/disable (non-reloadable).

## Write Routing

The `routing_strategy` setting controls how the orchestrator selects a backend for new objects (PutObject, CopyObject, CreateMultipartUpload):

- **pack** (default) — fills the first backend in config order until its quota is full, then overflows to the next. Good for maximizing usable capacity on free-tier providers where you want to fill one allocation before touching the next.
- **spread** — places each object on the backend with the lowest utilization ratio (`(bytes_used + orphan_bytes) / bytes_limit`). Good for distributing storage evenly across backends to balance load and wear.

Both strategies respect quota limits — a backend with no remaining space is skipped regardless of strategy. When usage limits are configured, backends that have exceeded their monthly limits are also excluded from selection.

## Rebalancing

The rebalancer periodically moves objects between backends to optimize storage distribution. Disabled by default to avoid unexpected egress charges.

Two strategies:

- **pack** - Fills backends in configuration order, consolidating free space on the last backend. Good for maximizing usable capacity on free-tier providers.
- **spread** - Equalizes utilization ratios across all backends. Good for distributing load evenly.

The `threshold` parameter (0–1) sets the minimum utilization spread required to trigger a rebalance run. Objects are moved in configurable batch sizes with bounded concurrency (`concurrency` setting, default 5) for throughput.

## Replication

When `replication.factor` is greater than 1, a background worker creates additional copies of objects on different backends to reach the target factor. Read operations automatically fail over to replicas if the primary copy is unavailable.

The worker runs once at startup to catch up on pending replicas, then continues at the configured interval.

### Health-Aware Replication

When backend circuit breakers are enabled, the replication worker is aware of backend health. If a backend's circuit breaker has been open longer than `unhealthy_threshold` (default: 10 minutes), the replicator treats copies on that backend as unavailable and creates replacement copies on healthy backends to maintain the target replication factor.

This prevents a sustained backend outage from silently reducing effective redundancy. For example, with `factor: 2` and an object on backends A and B, if B goes down and stays down past the threshold, the replicator creates a third copy on backend C — restoring two accessible copies.

The threshold prevents replacement copies from being created during brief transient failures. The replicator also prefers healthy backends as copy sources and never selects a circuit-broken backend as a replication target.

When a backend recovers, the extra copies it created are cleaned up automatically by the over-replication cleaner (see [Over-Replication Cleanup](#over-replication-cleanup)).

## Over-Replication Cleanup

When a backend recovers after the replicator has already created replacement copies on other backends, objects end up with more copies than the replication factor. A background worker detects and removes the excess.

The cleaner scores each copy by its backend's health and storage utilization, then removes the lowest-scoring copies until the object reaches the target factor:

- **Draining backend**: score 0 (always removed first)
- **Circuit-broken backend**: score 1 (removed next)
- **Healthy backend**: score 2 + (1 − utilization ratio), range [2..3]

Among healthy backends, the most utilized backend gets the lowest score — freeing space where it is scarcest. Each object's copies are locked with `FOR UPDATE` to prevent races with concurrent replicator or rebalancer activity.

The worker runs at the `replication.worker_interval` and shares the same `batch_size` and `concurrency` settings. It only runs when `replication.factor > 1`. Like the replicator, it uses a PostgreSQL advisory lock for multi-instance coordination.

Cleanup can also be triggered on demand via the admin API (`POST /admin/api/over-replication`), the CLI (`s3-orchestrator admin over-replication --execute`), or the web dashboard's **Clean Excess** button.

## Cleanup Queue

When a backend S3 operation succeeds but the subsequent metadata update or cleanup deletion fails, an orphaned object is left on the backend — invisible to the system, consuming storage but not tracked by quotas. Rather than silently logging these failures, the orchestrator enqueues them in a persistent `cleanup_queue` table in PostgreSQL for automatic retry.

**Orphan bytes tracking** — each enqueued item records the object's `size_bytes`. When an item is enqueued, the corresponding backend's `orphan_bytes` counter in `backend_quotas` is incremented. All capacity checks (write routing, replication target selection) subtract `orphan_bytes` from available space, so the write path never overcommits storage on a backend with pending cleanups. When a cleanup succeeds, `orphan_bytes` is decremented. This prevents a sustained backend outage from silently allowing quota overcommitment: even if a backend is down for days and cleanup retries are exhausting, the space consumed by orphaned objects remains reserved.

A background worker runs every minute, fetching pending items and attempting to delete them from their respective backends. Failed attempts are rescheduled with exponential backoff (`1m × 2^attempts`, capped at 24h). After 10 failed attempts, the item remains in the queue for manual inspection — the worker query filters it out via a partial index, and `orphan_bytes` stays incremented to keep the space reserved.

Enqueue points cover all failure sites across the codebase:

- **PutObject / CopyObject / CompleteMultipartUpload** — orphaned object when `RecordObject` fails and the immediate cleanup delete also fails
- **PutObject / CopyObject (overwrite)** — displaced copies on other backends when a key is overwritten; old copies that can't be immediately deleted are enqueued
- **DeleteObject** — metadata removed but backend delete fails (storage leak)
- **UploadPart** — part uploaded but `RecordPart` fails and cleanup delete fails
- **CompleteMultipartUpload / AbortMultipartUpload** — temporary `__multipart/` part objects not deleted
- **Rebalancer** — orphaned copy on destination when `MoveObjectLocation` fails, or stale source copy after a successful move
- **Replicator** — orphaned replica when `RecordReplica` fails or source is deleted during replication

Enqueue is best-effort: if the database is down (circuit breaker open), the failure is logged and the orphan is not enqueued. This avoids cascading failures — if the DB recovers, the next operation that fails will be enqueued normally.

Operators can inspect exhausted items directly:

```sql
SELECT * FROM cleanup_queue WHERE attempts >= 10;

-- After manually resolving, decrement orphan_bytes and remove the item:
UPDATE backend_quotas SET orphan_bytes = orphan_bytes - (SELECT size_bytes FROM cleanup_queue WHERE id = 123) WHERE backend_name = (SELECT backend_name FROM cleanup_queue WHERE id = 123);
DELETE FROM cleanup_queue WHERE id = 123;
```

## Lifecycle (Object Expiration)

Config-driven lifecycle rules automatically delete objects matching a key prefix after a configurable number of days. Useful for expiring temporary uploads, staging artifacts, or any objects with a known retention period.

```yaml
lifecycle:
  rules:
    - prefix: "tmp/"
      expiration_days: 7
    - prefix: "uploads/staging/"
      expiration_days: 1
```

A background worker runs hourly and evaluates each rule against `created_at` timestamps in the `object_locations` table (uses an existing index — no schema changes needed). Deletions go through the standard `DeleteObject` path, so all copies are removed, quotas are decremented, and failed backend deletes are enqueued to the cleanup queue.

Rules are hot-reloadable via `SIGHUP`. An empty rules list (or omitting the section entirely) disables lifecycle — no advisory lock is acquired and no DB queries are executed.

## Orphan Reconciliation

Optional background service that periodically scans each backend's S3 bucket and imports untracked objects into the metadata database. Objects the proxy doesn't know about — orphans from failed writes where both the DB record and cleanup queue entry were lost, manually uploaded objects, or artifacts from a database restore — are automatically brought under management so quota accounting stays accurate.

The reconciler uses the same `SyncBackend` path as the `sync` CLI subcommand: for each backend, it calls `ListObjects`, checks each key against the database, and calls `ImportObject` for any that aren't tracked. Already-managed objects are skipped. The first configured virtual bucket is used as the prefix for objects that don't already have a bucket prefix.

```yaml
reconcile:
  enabled: true       # disabled by default
  interval: "24h"     # how often to run (default: 24h)
```

Disabled by default. Requires a restart to enable/disable (non-reloadable). Runs under advisory lock `1009` to prevent concurrent scans across instances.

## Encryption

Optional server-side envelope encryption with AES-256-GCM. When enabled, every object is encrypted before it leaves the orchestrator — backends only ever see ciphertext. Each object gets a random 256-bit Data Encryption Key (DEK) that is wrapped by a master key before storage. The master key can come from an inline config value, a file on disk, or HashiCorp Vault Transit.

Objects are encrypted in fixed-size chunks (default 64 KB), so range requests (`Range` header) work without downloading the entire object — the orchestrator calculates which ciphertext chunks to fetch and decrypts only those. Clients see standard S3 behavior; encryption is fully transparent.

**Key features:**
- **Chunked AES-256-GCM** — each chunk has an independent nonce derived from a base nonce XORed with the chunk index, enabling random-access decryption
- **Envelope encryption** — per-object DEKs mean rotating the master key only requires re-wrapping DEKs, not re-encrypting data
- **Key rotation** — add the new master key, move the old one to `previous_keys`, and call the `rotate-encryption-key` admin API to re-wrap DEKs still using the old key
- **Encrypt existing data** — the `encrypt-existing` admin API encrypts all unencrypted objects in-place without downtime
- **Decrypt existing data** — the `decrypt-existing` admin API reverses encryption, restoring plaintext objects on backends (useful for disabling encryption or migrating away)
- **Vault Transit support** — delegate key management to HashiCorp Vault for HSM-backed key storage. The Vault token is automatically renewed in the background; for Nomad workload identity deployments, use `token_file` to point at the Nomad-managed token file instead of a static `token` string
- **Unknown key ID detection** — when a wrapped DEK references a key ID that isn't the current primary or any configured previous key, a warning is logged before falling back to the primary key (signals potential metadata corruption or missing rotation key)

**Compatibility with backend-side encryption:** If your backend already has its own server-side encryption (e.g., AWS SSE-S3 or SSE-KMS), both layers work independently. The orchestrator encrypts before uploading and the backend encrypts the ciphertext again at rest. On read, the backend decrypts its layer and returns the orchestrator's ciphertext, which the orchestrator then decrypts. This is harmless but redundant — you can safely disable the backend's encryption to avoid unnecessary KMS costs.

See the [Admin Guide](docs/admin-guide.md#encryption) for setup, key rotation, and encrypting existing data.

## Object Data Cache

Optional in-memory LRU cache that stores full GET responses to reduce backend API calls and egress. When a cached object is requested, the response is served directly from memory without contacting the backend. Useful for read-heavy workloads where the same objects are fetched repeatedly.

**Key behaviors:**
- **Full GET responses only** — range requests bypass the cache on miss but are served from cache on hit
- **Admission control** — objects larger than `max_object_size` are never cached, preventing a single large object from evicting many smaller ones
- **Automatic invalidation** — cache entries are evicted on PutObject, DeleteObject, CopyObject, DeleteObjects, and CompleteMultipartUpload
- **TTL-based expiry** — entries expire after the configured TTL regardless of access, bounding staleness in multi-instance deployments where writes may happen on another instance
- **Per-instance** — each orchestrator instance maintains its own cache; caches are not shared across instances
- **Post-decryption** — when encryption is enabled, the cache stores decrypted plaintext (same security properties as any in-process data)

```yaml
cache:
  enabled: true
  max_size: "256MB"          # total cache capacity
  max_object_size: "10MB"    # largest object eligible for caching
  ttl: "5m"                  # time-to-live per entry
```

Disabled by default. Requires a restart to enable/disable (non-reloadable).

## Rate Limiting

Optional per-IP token bucket rate limiting. When enabled, requests exceeding the configured rate return `429 SlowDown` with a `Retry-After: 1` header. Stale IP entries are evicted by a background goroutine every `cleanup_interval` (default 1m); entries not seen within `cleanup_max_age` (default 5m) are removed. Under high source-IP cardinality (e.g., DDoS), the map can accumulate up to `cleanup_max_age` worth of unique IPs before eviction runs — tune these values if memory pressure is a concern.

When running behind a reverse proxy (e.g., Traefik, nginx), configure `trusted_proxies` with the proxy's CIDR ranges so the orchestrator extracts the real client IP from the `X-Forwarded-For` header using rightmost-untrusted extraction. Without `trusted_proxies`, `X-Forwarded-For` is ignored and the direct connection IP is always used.

## Usage Limits

Per-backend monthly limits for API requests, egress bytes, and ingress bytes. Set any limit to `0` (or omit it) for unlimited. Limits reset naturally each month — the usage tracking table is keyed by `YYYY-MM` period.

**Enforcement behavior:**

- **Writes** (PutObject, CopyObject, CreateMultipartUpload, UploadPart) — backends over their limits are excluded from selection; writes overflow to the next eligible backend. If all backends are over-limit, the orchestrator returns `507 InsufficientStorage`.
- **Reads** (GetObject, HeadObject) — over-limit backends are skipped; the orchestrator tries replicas. Returns `429 SlowDown` only when *all* copies of the object are on over-limit backends.
- **Deletes** (DeleteObject, DeleteObjects, AbortMultipartUpload) — always allowed regardless of limits.

Effective usage is computed as `DB baseline + unflushed counters + proposed operation`, so enforcement stays accurate between flush/refresh cycles without double-counting. The flush interval is configurable (default 30s) and can adaptively shorten when backends approach their limits. For multi-instance deployments, optional [Redis shared counters](#usage-counters) eliminate the cross-instance blind spot between flushes.

## Configuration

YAML config file specified via `-config` flag (default: `config.yaml`). Supports `${ENV_VAR}` expansion.

```yaml
server:
  listen_addr: "0.0.0.0:9000"
  max_object_size: 5368709120  # 5 GB (default)
  # max_concurrent_requests: 0  # total concurrent operations — HTTP + background services (0 = unlimited, default: 1000)
  # max_concurrent_reads: 0     # separate read concurrency limit (0 = use global)
  # max_concurrent_writes: 0    # separate write concurrency limit (0 = use global; background services share this budget)
  # load_shed_threshold: 0      # active shedding at this capacity ratio (0 = disabled)
  # admission_wait: "0s"        # brief wait before rejection (0 = instant)
  # backend_timeout: "30s"       # per-operation timeout for backend S3 calls (default: 30s; uses tighter of this or parent context deadline)
  # read_header_timeout: "10s"   # max time to read request headers (default: 10s)
  # read_timeout: "5m"           # max time to read entire request including body (default: 5m)
  # write_timeout: "5m"          # max time to write response (default: 5m)
  # idle_timeout: "120s"         # max time to wait for next request on keep-alive (default: 120s)
  # shutdown_delay: "0s"         # delay before toggling readiness off and draining HTTP (default: 0; LB continues routing during delay)
  # tls:
  #   cert_file: "/path/to/cert.pem"  # hot-reloaded on SIGHUP; warns if cert expires within 24h
  #   key_file: "/path/to/key.pem"
  #   min_version: "1.2"           # "1.2" (default) or "1.3"
  #   client_ca_file: ""           # CA bundle for mTLS client verification

# Virtual buckets with per-bucket credentials
buckets:
  - name: "app1-files"
    # max_multipart_uploads: 100  # optional; limit active multipart uploads per bucket (0 = unlimited)
    credentials:
      - access_key_id: "APP1_ACCESS_KEY"
        secret_access_key: "APP1_SECRET_KEY"

  - name: "shared-files"
    credentials:
      # Multiple services can share a bucket with separate credentials
      - access_key_id: "WRITER_ACCESS_KEY"
        secret_access_key: "WRITER_SECRET_KEY"
      - access_key_id: "READER_ACCESS_KEY"
        secret_access_key: "READER_SECRET_KEY"

  # Legacy token auth (backward compatibility)
  # - name: "legacy-bucket"
  #   credentials:
  #     - token: "my-secret-token"

# SQLite (default) — zero-dependency, single-instance
database:
  driver: sqlite
  path: "s3-orchestrator.db"

# PostgreSQL — required for multi-instance deployments
# database:
#   driver: postgres
#   host: "localhost"
#   port: 5432
#   database: "s3proxy"
#   user: "s3proxy"
#   password: "secret"
#   ssl_mode: "require"
#   max_conns: 50             # default: 50; size to 2-3x max_concurrent_requests
#   min_conns: 10
#   max_conn_lifetime: "5m"

routing_strategy: "pack"       # "pack" (fill in order) or "spread" (least utilized) (default: pack)

backends:
  - name: "oci"
    endpoint: "https://namespace.compat.objectstorage.region.oraclecloud.com"
    region: "us-phoenix-1"
    bucket: "my-bucket"
    access_key_id: "backend-access-key"
    secret_access_key: "backend-secret-key"
    force_path_style: true
    unsigned_payload: true    # stream uploads without buffering (auto-enabled for HTTPS, set explicitly for HTTP)
    disable_checksum: false   # disable SDK default checksums for GCS and other providers that reject them
    strip_sdk_headers: false  # strip AWS SDK v2 headers before signing for GCS compatibility
    quota_bytes: 21474836480  # 20 GB (0 or omit for unlimited)
    api_request_limit: 0      # monthly API request limit (0 = unlimited)
    egress_byte_limit: 0      # monthly egress byte limit (0 = unlimited)
    ingress_byte_limit: 0     # monthly ingress byte limit (0 = unlimited)

**Provider quick reference** — endpoint format and required flags for common S3-compatible providers:

| Provider | Endpoint | `force_path_style` | Notes |
|----------|----------|-------------------|-------|
| AWS S3 | `https://s3.<region>.amazonaws.com` | `false` (default) | |
| MinIO | `http://<host>:9000` | `true` | |
| OCI Object Storage | `https://<ns>.compat.objectstorage.<region>.oraclecloud.com` | `true` | |
| Backblaze B2 | `https://s3.<region>.backblazeb2.com` | `false` | |
| Cloudflare R2 | `https://<account-id>.r2.cloudflarestorage.com` | `false` | `region: auto` |
| Wasabi | `https://s3.<region>.wasabisys.com` | `false` | |
| Google Cloud Storage | `https://storage.googleapis.com` | `false` | Set `disable_checksum: true` and `strip_sdk_headers: true` |

See the [Maximizing Free Tiers](https://s3-orchestrator.munchbox.cc/guides/maximizing-free-tiers/) guide for detailed setup on each provider including where to find credentials.

telemetry:
  metrics:
    enabled: true
    path: "/metrics"
    # listen: "127.0.0.1:9091"  # optional; serve metrics on a separate address (recommended for production)
  tracing:
    enabled: true
    endpoint: "localhost:4317"
    insecure: true
    sample_rate: 1.0             # fraction of requests that generate OTel traces (use 0.01–0.1 in production)

circuit_breaker:
  failure_threshold: 3     # consecutive DB failures before opening (default: 3)
  open_timeout: "15s"      # delay before probing recovery (default: 15s)
  cache_ttl: "60s"         # key→backend cache TTL during degraded reads (default: 60s)
  parallel_broadcast: false # fan-out reads to all backends in parallel during degraded mode (default: false)

# backend_circuit_breaker:   # per-backend circuit breakers (disabled by default)
#   enabled: false
#   failure_threshold: 5     # consecutive failures before opening (default: 5)
#   open_timeout: "5m"       # delay before probing recovery (default: 5m)

rebalance:
  enabled: false
  strategy: "pack"         # "pack" or "spread" (default: pack)
  interval: "6h"           # run interval (default: 6h)
  batch_size: 100          # max objects per run (default: 100)
  threshold: 0.1           # min utilization spread to trigger (default: 0.1)
  concurrency: 5           # parallel moves per run (default: 5)

replication:
  factor: 1                # copies per object; 1 = no replication (default: 1)
  worker_interval: "5m"    # replication worker cycle (default: 5m)
  batch_size: 50           # objects per cycle (default: 50)
  concurrency: 5           # parallel replications per cycle (default: 5)
  unhealthy_threshold: "10m" # grace period before replacing copies on circuit-broken backends (default: 10m)

cleanup_queue:
  concurrency: 10          # parallel cleanup deletions per tick (default: 10)

rate_limit:
  enabled: false
  requests_per_sec: 100    # token refill rate (default: 100)
  burst: 200               # max burst size (default: 200)
  cleanup_interval: "1m"   # stale entry eviction interval (default: 1m)
  cleanup_max_age: "5m"    # evict entries not seen within this window (default: 5m)
  # trusted_proxies:       # CIDRs whose X-Forwarded-For is trusted
  #   - "10.0.0.0/8"       # Uses rightmost-untrusted extraction
  #   - "172.16.0.0/12"

encryption:
  enabled: false
  # chunk_size: 65536           # plaintext bytes per chunk (default: 64KB, range: 4KB–1MB, power of 2)
  # master_key: "${ENCRYPTION_KEY}"  # base64-encoded 256-bit key (exactly one key source required)
  # master_key_file: "/path/to/key"  # alternative: raw 32-byte key file
  # vault:                           # alternative: Vault Transit
  #   address: "http://vault:8200"
  #   token: "${VAULT_TOKEN}"        # static token (auto-renewed via RenewSelf)
  #   # token_file: "/secrets/vault-token"  # OR file-based (for Nomad workload identity; re-read periodically)
  #   key_name: "s3-orchestrator"
  #   mount_path: "transit"          # default: transit
  #   # ca_cert: "/path/to/ca.pem"  # Vault CA certificate for TLS verification
  #   # renew_interval: "5m"        # token renewal check interval (default: 5m)
  # previous_keys:                   # old master keys for rotation (unwrap only)
  #   - "base64-encoded-old-key"

integrity:
  enabled: false               # SHA-256 content hashing for data integrity verification
  # verify_on_read: false      # hash-check GET responses as they stream
  # verify_on_replicate: true  # verify hash when creating replicas (default: true when enabled)
  # scrubber_interval: "6h"    # background verification interval (0 = disabled)
  # scrubber_batch_size: 100   # objects per scrub cycle

# cache:                        # optional: in-memory LRU object data cache
#   enabled: false               # disabled by default
#   max_size: "256MB"            # total cache capacity (default: 256MB)
#   max_object_size: "10MB"      # largest cacheable object (default: 10MB)
#   ttl: "5m"                    # per-entry time-to-live (default: 5m)

ui:
  enabled: false             # enable the built-in web dashboard
  path: "/ui"                # URL prefix (default: /ui)
  admin_key: "${UI_ADMIN_KEY}"       # access key for dashboard login
  admin_secret: "${UI_ADMIN_SECRET}" # secret key (plaintext or bcrypt hash)
  session_secret: "${UI_SESSION_SECRET}" # required — HMAC key for session cookies (independent of admin_secret)
  # admin_token: ""          # separate token for admin API (defaults to admin_key)
  # force_secure_cookies: false # always set Secure flag on cookies (for behind TLS proxy)

usage_flush:
  interval: "30s"            # base flush interval (default: 30s)
  adaptive_enabled: false    # shorten interval when near usage limits (default: false)
  adaptive_threshold: 0.8    # usage ratio to trigger fast flush (default: 0.8)
  fast_interval: "5s"        # interval when near limits (default: 5s)

# reconcile:                   # optional: periodic orphan reconciliation
#   enabled: false             # scan backends for untracked objects (default: false)
#   interval: "24h"            # how often to run (default: 24h)

# redis:                       # optional: shared usage counters for multi-instance deployments
#   address: "redis:6379"      # host:port (required when section is present)
#   password: ""               # AUTH password (omit for no auth)
#   db: 0
#   tls: false
#   key_prefix: "s3orch"       # namespace for multi-tenant Redis (default: s3orch)
#   failure_threshold: 3       # consecutive failures before local fallback (default: 3)
#   open_timeout: "15s"        # delay before probing recovery (default: 15s)

lifecycle:
  rules:                       # empty or omitted = lifecycle disabled
    - prefix: "tmp/"           # key prefix to match
      expiration_days: 7       # delete objects older than this
    - prefix: "uploads/staging/"
      expiration_days: 1
```

## Configuration Hot-Reload

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
| `server` timeouts | No | `read_header_timeout`, `read_timeout`, `write_timeout`, `idle_timeout`, `shutdown_delay` |
| `server.tls` | No | Requires restart |
| `database` | No | Requires restart |
| `telemetry` | No | Requires restart |
| `circuit_breaker` | No | Requires restart |
| `backend_circuit_breaker` | No | Requires restart |
| `ui` | No | Requires restart |
| `encryption` | No | Requires restart |
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

## Database

The orchestrator connects to PostgreSQL via pgx/v5 connection pools and auto-applies versioned migrations on startup using [goose](https://github.com/pressly/goose). Migration files are embedded in the binary and tracked via a `goose_db_version` table — only unapplied migrations run. Six tables are created:

| Table | Purpose |
|-------|---------|
| `backend_quotas` | Per-backend byte limits, usage counters, and orphan bytes tracking |
| `object_locations` | Maps object keys to backends with size tracking |
| `multipart_uploads` | In-progress multipart upload metadata |
| `multipart_parts` | Individual parts for active multipart uploads |
| `backend_usage` | Monthly per-backend API request and data transfer counters |
| `cleanup_queue` | Retry queue for failed backend object deletions |

Quota updates are transactional: object location inserts/deletes and quota counter changes happen atomically.

All SQL queries live in `internal/store/sqlc/queries/` as annotated `.sql` files. Type-safe Go code is generated by sqlc into `internal/store/sqlc/`. To regenerate after editing queries:

```bash
make generate
```

## Telemetry

### Prometheus Metrics

All metrics are prefixed with `s3o_`. Exposed at `/metrics` when enabled.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `s3o_build_info` | Gauge | version, go_version | Build metadata |
| `s3o_requests_total` | Counter | method, status_code | HTTP request count |
| `s3o_request_duration_seconds` | Histogram | method | Request latency |
| `s3o_request_size_bytes` | Histogram | method | Upload sizes |
| `s3o_response_size_bytes` | Histogram | method | Download sizes |
| `s3o_inflight_requests` | Gauge | method | Currently processing |
| `s3o_backend_requests_total` | Counter | operation, backend, status | Backend S3 API calls |
| `s3o_backend_duration_seconds` | Histogram | operation, backend | Backend latency |
| `s3o_manager_requests_total` | Counter | operation, backend, status | Manager-level operations |
| `s3o_manager_duration_seconds` | Histogram | operation, backend | Manager latency |
| `s3o_quota_bytes_used` | Gauge | backend | Current bytes used |
| `s3o_quota_bytes_limit` | Gauge | backend | Quota limit |
| `s3o_quota_orphan_bytes` | Gauge | backend | Bytes reserved by pending cleanup items |
| `s3o_quota_bytes_available` | Gauge | backend | Remaining space (limit − used − orphan) |
| `s3o_objects_count` | Gauge | backend | Stored object count |
| `s3o_active_multipart_uploads` | Gauge | backend | In-progress uploads |
| `s3o_rebalance_objects_moved_total` | Counter | strategy, status | Objects moved by rebalancer |
| `s3o_rebalance_bytes_moved_total` | Counter | strategy | Bytes moved by rebalancer |
| `s3o_rebalance_runs_total` | Counter | strategy, status | Rebalancer executions |
| `s3o_rebalance_duration_seconds` | Histogram | strategy | Rebalancer execution time |
| `s3o_rebalance_skipped_total` | Counter | reason | Rebalancer runs skipped |
| `s3o_rebalance_pending` | Gauge | — | Objects planned for rebalance |
| `s3o_replication_pending` | Gauge | — | Objects below replication factor |
| `s3o_replication_copies_created_total` | Counter | — | Replica copies created |
| `s3o_replication_errors_total` | Counter | — | Replication errors |
| `s3o_replication_duration_seconds` | Histogram | — | Replication cycle time |
| `s3o_replication_runs_total` | Counter | status | Replication worker executions |
| `s3o_replication_health_copies_total` | Counter | — | Copies created to replace copies on circuit-broken backends |
| `s3o_over_replication_pending` | Gauge | — | Objects exceeding the replication factor |
| `s3o_over_replication_removed_total` | Counter | — | Excess copies removed |
| `s3o_over_replication_errors_total` | Counter | — | Over-replication cleanup errors |
| `s3o_over_replication_runs_total` | Counter | status | Over-replication worker executions |
| `s3o_over_replication_duration_seconds` | Histogram | — | Over-replication cleanup cycle time |
| `s3o_circuit_breaker_state` | Gauge | name | 0=closed, 1=open, 2=half-open (name: "database" or backend name) |
| `s3o_circuit_breaker_transitions_total` | Counter | name, from, to | State transitions per component |
| `s3o_degraded_reads_total` | Counter | operation | Broadcast reads in degraded mode |
| `s3o_degraded_cache_hits_total` | Counter | — | Cache hits during degraded reads |
| `s3o_degraded_write_rejections_total` | Counter | operation | Writes rejected in degraded mode |
| `s3o_usage_api_requests` | Gauge | backend | Current month API request count |
| `s3o_usage_egress_bytes` | Gauge | backend | Current month egress bytes |
| `s3o_usage_ingress_bytes` | Gauge | backend | Current month ingress bytes |
| `s3o_usage_limit_rejections_total` | Counter | operation, limit_type | Operations rejected by usage limits |
| `s3o_cleanup_queue_enqueued_total` | Counter | reason | Items added to the cleanup retry queue |
| `s3o_cleanup_queue_processed_total` | Counter | status | Items processed from the cleanup queue (success/retry/exhausted) |
| `s3o_cleanup_queue_depth` | Gauge | — | Current pending items in the cleanup queue |
| `s3o_rate_limit_rejections_total` | Counter | — | Requests rejected by per-IP rate limiting |
| `s3o_admission_rejections_total` | Counter | — | Requests rejected by server-level admission control |
| `s3o_lifecycle_deleted_total` | Counter | — | Objects deleted by lifecycle expiration |
| `s3o_lifecycle_failed_total` | Counter | — | Objects that failed lifecycle deletion |
| `s3o_lifecycle_runs_total` | Counter | status | Lifecycle worker executions |
| `s3o_audit_events_total` | Counter | event | Audit log entries emitted |
| `s3o_drain_active` | Gauge | — | `1` while a backend drain is in progress |
| `s3o_drain_objects_moved_total` | Counter | — | Objects migrated during drain |
| `s3o_drain_bytes_moved_total` | Counter | — | Bytes migrated during drain |
| `s3o_encryption_operations_total` | Counter | op | Encrypt/decrypt operations (encrypt, decrypt, decrypt_range) |
| `s3o_encryption_errors_total` | Counter | op, error_type | Encryption/decryption failures |
| `s3o_encryption_unknown_key_id_total` | Counter | — | Decryption attempts with unknown keyID (primary key fallback) |
| `s3o_encrypt_existing_objects_total` | Counter | status | Objects processed by encrypt-existing (success/error) |
| `s3o_decrypt_existing_objects_total` | Counter | status | Objects processed by decrypt-existing (success/error) |
| `s3o_key_rotation_objects_total` | Counter | status | DEKs re-wrapped by key rotation (success/error) |
| `s3o_redis_operations_total` | Counter | operation, status | Redis command outcomes (incrby, get, getset, pipeline_add, pipeline_load) |
| `s3o_redis_fallback_active` | Gauge | — | `1` when Redis is unavailable and using local counters |
| `s3o_cache_hits_total` | Counter | — | Object data cache hits |
| `s3o_cache_misses_total` | Counter | — | Object data cache misses |
| `s3o_cache_evictions_total` | Counter | — | Object data cache evictions (LRU or TTL) |
| `s3o_cache_size_bytes` | Gauge | — | Current memory used by cached objects |
| `s3o_cache_entries` | Gauge | — | Current number of cached objects |
| `s3o_integrity_checks_total` | Counter | operation | Integrity hash verifications performed (read, scrub) |
| `s3o_integrity_errors_total` | Counter | operation | Hash mismatches detected (corrupted copies enqueued for cleanup) |

Quota metrics are refreshed from PostgreSQL every 30 seconds (no backend API calls).

A ready-to-import Grafana dashboard covering all metrics is included at `grafana/s3-orchestrator.json`.

### OpenTelemetry Tracing

Spans are emitted for every HTTP request, manager operation, and backend S3 call. The service registers as `s3-orchestrator` (`resource.service.name`). Traces propagate via W3C `traceparent` headers. Configured to export via gRPC OTLP to Tempo or any OTLP-compatible collector.

**Trace-to-log correlation** — every JSON log line emitted within an active span automatically includes `trace_id` and `span_id` fields. Log aggregators (Loki, etc.) can use these fields to link logs to their corresponding traces in Tempo or any OpenTelemetry-compatible tracing backend. Only log calls that receive a `context.Context` with an active span include trace context; application-level logs without a span context are unaffected.

### Audit Logging

Structured audit log entries are emitted as JSON via `slog` for every S3 API request and significant internal operation. Each entry includes an `"audit": true` marker for easy filtering in log pipelines.

**Request ID tracing** — every S3 API request gets a unique request ID, returned in the `X-Amz-Request-Id` response header. Clients can supply their own via the `X-Request-Id` request header. The same ID flows through context to all downstream operations, appearing in both the HTTP-level audit entry and the storage-level audit entry for full request correlation. The ID is also set as a `s3o.request_id` attribute on OpenTelemetry spans, linking audit logs to traces.

**Two-level audit entries** — each S3 request produces two audit log lines: one at the HTTP layer (`s3.PutObject`, `s3.GetObject`, etc.) with method, path, bucket, status, duration, and remote address, and one at the storage layer (`storage.PutObject`, `storage.GetObject`, etc.) with the backend name, object key, and size. Both share the same `request_id`.

**Internal operation auditing** — background operations generate their own correlation IDs:

| Operation | Events |
|-----------|--------|
| Rebalancer | `rebalance.start`, `rebalance.move`, `rebalance.complete` |
| Replicator | `replication.start`, `replication.copy`, `replication.complete` |
| Over-replication cleaner | `over_replication.start`, `over_replication.remove`, `over_replication.complete` |
| Multipart cleanup | `storage.MultipartCleanup` |
| Overwrite (displaced) | `storage.overwrite_displaced` |
| Cleanup queue | `cleanup_queue.processed` |

Example audit log entry:

```json
{"level":"INFO","msg":"audit","audit":true,"event":"s3.PutObject","request_id":"a1b2c3d4e5f6...","operation":"PutObject","method":"PUT","path":"/my-files/photo.jpg","bucket":"my-files","status":200,"duration":"45ms"}
```

## Web UI

A built-in web dashboard provides operational visibility and management without external tooling. When enabled, it renders a server-side HTML page at the configured path (default `/ui/`). All routes require authentication via HMAC-signed session cookies — users log in with an admin key/secret pair configured in the YAML config.

The dashboard shows:

- **Storage Summary** — total bytes used/capacity across all backends with a progress bar
- **Backends** — quota used/limit per backend with progress bars, object counts, active multipart uploads
- **Monthly Usage** — API requests, egress, and ingress per backend with limits
- **Objects** — interactive collapsible tree browser; buckets and directories expand on click to reveal contents, with rollup file counts and sizes
- **Configuration** — virtual buckets, write routing strategy, replication factor, rebalance strategy, rate limit status
- **Logs** — recent structured log output from an in-memory ring buffer (last 5,000 entries), filterable by severity level with client-side text search and optional auto-refresh

The dashboard also provides management actions:

- **Upload** — upload files to any virtual bucket via the browser
- **Download** — download individual objects from the file tree
- **Delete** — delete individual objects from the file tree
- **Rebalance** — trigger an on-demand rebalance across backends
- **Clean Excess** — remove over-replicated copies that exceed the replication factor
- **Sync** — import pre-existing objects from a backend's S3 bucket into the proxy database, scoped to a selected virtual bucket

The object tree uses JavaScript for lazy-loaded AJAX expansion — directories load their children on click via the `/ui/api/tree` endpoint. All dashboard responses include security headers (`X-Frame-Options`, `X-Content-Type-Options`, `Referrer-Policy`, `Content-Security-Policy`). Enable it in the config:

```yaml
ui:
  enabled: true
  path: "/ui"                # default
  admin_key: "${UI_ADMIN_KEY}"
  admin_secret: "${UI_ADMIN_SECRET}"
```

JSON APIs are available at `{path}/api/dashboard`, `{path}/api/tree`, and `{path}/api/logs` for programmatic access. The logs endpoint accepts optional query parameters: `level`, `since`, `component`, and `limit`. Management endpoints (`{path}/api/delete`, `{path}/api/delete-prefix`, `{path}/api/upload`, `{path}/api/rebalance`, `{path}/api/clean-excess`, `{path}/api/sync`) accept POST requests and return JSON responses. The download endpoint (`{path}/api/download?key=...`) accepts GET requests.

## Endpoints

| Path | Purpose |
|------|---------|
| `/health` | Health check — returns `ok` or `degraded` (always 200) |
| `/metrics` | Prometheus metrics |
| `/ui/` | Web dashboard (when enabled) |
| `/ui/login` | Dashboard login page |
| `/ui/api/dashboard` | Dashboard data as JSON |
| `/ui/api/tree` | Lazy-loaded directory listing as JSON |
| `/ui/api/delete` | Delete an object (POST, JSON body) |
| `/ui/api/delete-prefix` | Delete all objects under a prefix (POST, JSON body) |
| `/ui/api/upload` | Upload a file (POST, multipart form) |
| `/ui/api/download` | Download a file (GET, query param: key) |
| `/ui/api/rebalance` | Trigger on-demand rebalance (POST) |
| `/ui/api/clean-excess` | Remove over-replicated copies (POST) |
| `/ui/api/logs` | Buffered log entries as JSON (query params: level, since, component, limit) |
| `/ui/api/sync` | Import objects from a backend (POST, JSON body) |
| `/{bucket}/{key}` | S3 API |

## Background Tasks

All locked background tasks apply a random startup jitter of up to half the tick interval before the first tick, preventing thundering herd on the advisory lock when multiple instances start simultaneously.

| Task | Interval | Advisory Lock | Description |
|------|----------|:-------------:|-------------|
| Usage flush + metrics | configurable (default 30s) | When Redis configured | Flushes usage counters to PostgreSQL, then refreshes quota stats, usage baselines, object counts, and multipart counts. Updates Prometheus gauges. Adaptive mode shortens interval near limits. Advisory lock is acquired whenever Redis is configured (regardless of health) to prevent double-counting during recovery. |
| Stale multipart cleanup | 1h | Yes | Aborts multipart uploads older than 24h and deletes their temporary part objects. |
| Cleanup queue | 1m | Yes | Retries failed backend object deletions with exponential backoff (1m to 24h, max 10 attempts). |
| Rebalancer | configurable (default 6h) | Yes | Moves objects between backends per strategy. Only runs when enabled. |
| Replicator | configurable (default 5m) | Yes | Creates copies of under-replicated objects. Only runs when factor > 1. Runs once at startup. |
| Over-replication cleaner | configurable (default 5m) | Yes | Removes excess copies of objects that exceed the replication factor. Only runs when factor > 1. |
| Lifecycle | 1h | Yes | Deletes objects matching lifecycle rules whose `created_at` exceeds `expiration_days`. Only runs when rules are configured. |
| Reconciler | configurable (default 24h) | Yes | Scans each backend for untracked objects and imports them into the metadata database via `SyncBackend`. Only runs when `reconcile.enabled: true`. |
| CB watchdog | 1m | No | Checks all circuit breakers for stale half-open probes. If a probe has been in flight longer than 2 minutes, resets the circuit to open so a new probe can be dispatched. Prevents circuits from getting stuck half-open when traffic stops. |

Background services (rebalancer, replicator, over-replication cleaner, cleanup queue) share the admission semaphore with HTTP requests, so `max_concurrent_requests` is the total budget for both HTTP and background backend operations.

## Multi-Instance Deployment

Multiple orchestrator instances can safely share the same PostgreSQL database. Background tasks (rebalancer, replicator, cleanup queue, multipart cleanup) use PostgreSQL advisory locks to prevent concurrent execution across instances — if one instance holds the lock for a task, other instances skip that tick silently.

Request-serving paths (PutObject, GetObject, etc.) are stateless and work correctly with any number of instances behind a load balancer. The per-instance location cache is TTL-bounded and self-correcting. Rate limiting remains per-instance.

### Usage Counters

Without Redis, each instance tracks usage counters independently in memory and flushes to PostgreSQL at the configured interval (default 30s). Between flushes, instances cannot see each other's accumulated usage, which can allow quota overshoot under high throughput.

With Redis configured, all instances share the same usage counters via Redis `INCRBY`/`GET` operations. The baseline+delta formula stays the same (`DB baseline + counter + proposed`), but the counter lives in Redis instead of local memory, eliminating the cross-instance blind spot. When Redis is active, only one instance flushes counters to PostgreSQL (coordinated via advisory lock) since `GETSET` is a destructive read.

A circuit breaker monitors Redis health. If Redis becomes unavailable, the backend falls back to local in-memory counters automatically — same behavior as running without Redis. A background health probe PINGs Redis periodically and, on recovery, syncs local deltas back to Redis via an additive INCRBY pipeline before resuming shared operation. The entire local counter map is swapped atomically (single pointer swap) so no concurrent Add calls can lose deltas between the snapshot and the pipeline. Stale Redis keys from before the outage expire via TTL. Local counters are zeroed only after the pipeline commits, so a crash mid-recovery cannot lose deltas. The recovery is safe for concurrent execution by multiple instances since INCRBY is additive.

```yaml
redis:
  address: "redis.example.com:6379"
  password: "${REDIS_PASSWORD}"
  key_prefix: "s3orch"       # namespace for multi-tenant Redis
  failure_threshold: 3        # consecutive failures before fallback
  open_timeout: "15s"         # delay before probing recovery
```

Redis is optional. Without it, adaptive flushing still shortens the flush interval when any backend approaches a usage limit, improving enforcement accuracy.

## CLI Subcommands

### version

Prints the binary version, Go version, and platform:

```bash
s3-orchestrator version
# s3-orchestrator v0.8.0 go1.26.0 linux/amd64
```

### validate

Validates a configuration file without starting the server. Exits 0 on success with a brief summary, or exits 1 with error details:

```bash
s3-orchestrator validate -config config.yaml
# config config.yaml: valid
#   backends: 2
#   buckets:  1
#   routing:  spread
```

### sync

Imports pre-existing objects from a backend S3 bucket into the orchestrator's metadata database. Useful when bringing an existing bucket under orchestrator management. The `--bucket` flag specifies which virtual bucket the imported objects belong to — keys are stored with a `{bucket}/` prefix for namespace isolation.

```bash
# Import all objects from a backend into the "unified" virtual bucket
s3-orchestrator sync --config config.yaml --backend oci --bucket unified

# Preview what would be imported
s3-orchestrator sync --config config.yaml --backend oci --bucket unified --dry-run

# Import only objects under a prefix
s3-orchestrator sync --config config.yaml --backend oci --bucket unified --prefix photos/
```

| Flag | Default | Description |
|------|---------|-------------|
| `--config` | `config.yaml` | Path to configuration file |
| `--backend` | (required) | Backend name to sync |
| `--bucket` | (required) | Virtual bucket name to prefix imported keys with |
| `--prefix` | `""` | Only sync objects with this key prefix |
| `--dry-run` | `false` | Preview what would be imported without writing |

Objects already tracked in the database for that backend are skipped. The command logs per-page progress and a final summary with imported count, skipped count, and total bytes imported.

### admin

Operational CLI for a running instance. Reads `config.yaml` to discover the server address and admin token. See the [Admin Guide](docs/admin-guide.md) for full details.

```bash
s3-orchestrator admin status                       # backend health and usage
s3-orchestrator admin object-locations -key "..."  # find all copies of an object
s3-orchestrator admin cleanup-queue                # cleanup queue depth
s3-orchestrator admin usage-flush                  # force flush usage counters
s3-orchestrator admin replicate                    # trigger replication cycle
s3-orchestrator admin over-replication             # show over-replicated object count
s3-orchestrator admin over-replication --execute   # clean excess copies
s3-orchestrator admin over-replication --execute --batch-size 200  # with custom batch
s3-orchestrator admin log-level                    # view current log level
s3-orchestrator admin log-level -set debug         # change log level at runtime
s3-orchestrator admin drain <backend>              # start draining a backend
s3-orchestrator admin drain-status <backend>       # check drain progress
s3-orchestrator admin drain-cancel <backend>       # cancel an active drain
s3-orchestrator admin remove-backend <backend>              # remove backend DB records (S3 objects preserved)
s3-orchestrator admin remove-backend <backend> --purge      # preview: shows what would be destroyed
s3-orchestrator admin remove-backend <backend> --purge --confirm  # delete S3 objects + DB records
```

## Development

```bash
# Install build and packaging dependencies
make tools

# Regenerate sqlc query code (after editing .sql files)
make generate

# Run locally (starts MinIO + PostgreSQL via Docker, then runs the server)
make run

# Lint
make lint

# Static analysis
make vet

# Scan Go dependencies for known vulnerabilities
make govulncheck

# Run unit tests
make test

# Run integration tests (requires Docker)
make integration-test

# Build local Docker image
make build

# Create a new database migration
make migration

# Build multi-arch and push to registry
make push VERSION=vX.Y.Z

# Build a .deb package for the host architecture
make deb VERSION=X.Y.Z

# Build .deb packages for both amd64 and arm64
make deb-all VERSION=X.Y.Z

# Build and run lintian validation
make deb-lint VERSION=X.Y.Z

# Publish .deb packages to an Aptly repository
make publish-deb

# Dry-run GoReleaser locally (builds everything without publishing)
make release-local
```

## Deployment

The orchestrator can run as a Docker container, a native systemd service, or on container orchestration platforms. Production-ready manifests for Nomad and Kubernetes are in [`deploy/`](deploy/), with local demo scripts that stand up a complete environment in one command.

### Container Orchestration (Nomad / Kubernetes)

Example manifests in `deploy/` demonstrate a three-backend setup with replication factor 2, spread routing, and full observability. Local demo scripts build from source and deploy against docker-compose backing services:

```bash
# Kubernetes via k3d (requires: docker, k3d, kubectl)
make kubernetes-demo

# Nomad in dev mode (requires: docker, nomad)
make nomad-demo
```

See [`deploy/README.md`](deploy/README.md) for production deployment instructions and customization options (TLS, mTLS, Vault integration, Ingress).

### Prerequisites

- PostgreSQL database (schema auto-applied on startup)
- At least one S3-compatible storage backend
- Configuration file with credentials
- Redis (optional — for shared usage counters in multi-instance deployments)
- **TLS termination** — either via the built-in `server.tls` config or a reverse proxy (Traefik, nginx, Ingress). Plain HTTP exposes SigV4 signatures and object data on the wire, and the `UNSIGNED-PAYLOAD` streaming mode means body integrity depends entirely on transport security. See the [Security Hardening](docs/security-hardening.md) guide for TLS and mTLS setup.

### Docker

Build and push a multi-arch image with a version tag:

```bash
make push VERSION=vX.Y.Z
```

The `VERSION` is baked into the binary via `-ldflags` and displayed in the web UI header and `/health` endpoint. Defaults to the value in `.version` if omitted.

### Debian Package

Build a `.deb` package for bare-metal or VM deployments:

```bash
make deb VERSION=X.Y.Z
```

Install and configure:

```bash
sudo dpkg -i s3-orchestrator_X.Y.Z_amd64.deb
sudo vim /etc/s3-orchestrator/config.yaml
sudo vim /etc/default/s3-orchestrator   # set DB_PASSWORD, backend keys, etc.
sudo systemctl start s3-orchestrator
```

The package installs:

| Path | Purpose |
|------|---------|
| `/usr/bin/s3-orchestrator` | Binary |
| `/etc/s3-orchestrator/config.yaml` | Configuration (conffile, preserved on upgrade) |
| `/etc/default/s3-orchestrator` | Environment variables for `${VAR}` expansion |
| `/usr/lib/systemd/system/s3-orchestrator.service` | Systemd unit |
| `/var/lib/s3-orchestrator/` | Data directory |

The systemd unit runs as a dedicated `s3-orchestrator` user with filesystem hardening (`ProtectSystem=strict`, `ProtectHome=yes`, `NoNewPrivileges=yes`). Config reload via `systemctl reload s3-orchestrator` sends `SIGHUP`.

### Releasing

Tag a version and push to trigger an automated GitHub Release via GoReleaser:

```bash
make release
```

This regenerates `CHANGELOG.md` via [git-cliff](https://git-cliff.org), tags the current `.version` value, and pushes the tag. The tag triggers GoReleaser to build Linux binaries (amd64 + arm64), Debian packages, and SHA256 checksums — all attached to the GitHub Release.

To regenerate the changelog without releasing:

```bash
make changelog
```

Commit categorization is configured in `cliff.toml`. Commit messages starting with `Add`, `Fix`, `Harden`, `Refactor`, `Improve`, `docs:`, `test:`, or `chore(deps):` are automatically grouped into the appropriate section.

Docker images are still built manually since the private registry isn't reachable from GitHub Actions:

```bash
make push VERSION=vX.Y.Z
```

To dry-run the release locally (builds everything without publishing):

```bash
make release-local
```

## Project Structure

```
cmd/s3-orchestrator/
  main.go                    Entry point, subcommand dispatch, backend init
  services.go                Background task lifecycle (tickers, advisory locks)
  admin.go                   Admin subcommand (operational CLI)
  sync.go                    Sync subcommand (bucket import)
  validate.go                Validate subcommand (config check)
  version.go                 Version subcommand (build info)
internal/
  transport/                 HTTP interface layer
    s3api/
      server.go              HTTP router, bucket resolution, key prefixing, metrics
      buckets.go             HeadBucket, GetBucketLocation, ListBuckets stubs
      objects.go             PUT, GET, HEAD, DELETE, COPY, DeleteObjects handlers
      list.go                ListObjectsV1 and ListObjectsV2 handlers (XML response)
      multipart.go           Multipart upload handlers
      helpers.go             Path parsing, S3 XML error responses
      ratelimit.go           Per-IP token bucket rate limiter
      admission.go           Request admission control with load shedding
    admin/handler.go         Admin API handler (status, cleanup queue, log level, etc.)
    auth/auth.go             BucketRegistry, SigV4 verification, legacy token auth
    ui/
      handler.go             Web UI HTTP handler, session auth + JSON APIs
      templates.go           Embedded templates + formatting helpers
      templates/             Dashboard and login HTML templates
      static/                CSS, JS (directory tree, log viewer)
    httputil/
      clientip.go            Client IP extraction with X-Forwarded-For + trusted proxies
      loginthrottle.go       Per-IP brute-force protection with lockout
      certreloader.go        TLS certificate hot-reload with expiry warning
  observe/                   Observability layer
    audit/audit.go           Request ID generation, context propagation, audit logger
    telemetry/
      metrics.go             Prometheus metric definitions
      tracing.go             OpenTelemetry tracer setup
      tracehandler.go        slog handler that injects trace_id/span_id from OTel context
      logbuffer.go           In-memory ring buffer + slog TeeHandler
    event/event.go           Notification event type constants and Emit hook
  util/                      Generic utilities
    bufpool/bufpool.go       Shared sync.Pool for streaming buffer reuse (io.CopyBuffer)
    syncutil/                AtomicConfig[T] and TTLCache[K,V]
    workerpool/              Generic bounded-concurrency Run[T] and Collect[T,R]
  config/                    YAML config loader split by domain (server, database, encryption, etc.)
  breaker/
    breaker.go               Generic three-state circuit breaker state machine
  backend/
    s3.go                    ObjectBackend interface, S3Backend (AWS SDK v2)
    circuitbreaker.go        Per-backend circuit breaker wrapper
  store/
    metadata.go              MetadataStore interface, narrow role interfaces, data types
    store.go                 PostgreSQL storage layer (pgx/v5 + sqlc)
    circuitbreaker.go        Database circuit breaker wrapper
    cleanup_queue.go         Cleanup queue Store methods
    migrations/              Versioned goose migration files (embedded in binary)
    sqlc/
      schema.sql             Schema for sqlc code generation
      queries/               Annotated SQL query files
      *.go                   Generated type-safe query code (do not edit)
  counter/
    counter.go               CounterBackend interface and field constants
    local.go                 In-memory atomic counter backend (default)
    redis.go                 Redis shared counter backend with circuit breaker fallback
    tracker.go               Usage limit enforcement, baseline management, flush
  proxy/
    manager.go               Composition root, config accessors
    objects.go               ObjectManager type, constructor, shared helpers
    objects_read.go          Read failover, broadcast reads, GetObject, HeadObject, ListObjects
    objects_write.go         PutObject, CopyObject, DeleteObject, DeleteObjects
    multipart.go             Multipart upload lifecycle
    drain.go                 Backend drain and remove operations
    core.go                  Shared infrastructure (timeout, admission, routing)
    lifecycle.go             Lifecycle expiration rule processing
    dashboard.go             DashboardData type + thin wrappers
    cache.go                 Key→backend cache with TTL + background eviction
    metrics.go               Prometheus metric recording + gauge refresh
    aggregator.go            Dashboard data aggregation + lazy directory listing
  worker/
    ops.go                   Role interfaces (StoreAccess, BackendAccess, AdmissionControl, DataMover)
    rebalancer.go            Object rebalancing across backends
    replicator.go            Cross-backend object replication
    overreplication.go       Over-replication detection and excess copy cleanup
    cleanup.go               Cleanup queue retry worker
    scrubber.go              Background integrity verification
    reconciler.go            Orphan discovery and import
  notify/
    notifier.go              Webhook notification delivery (outbox pattern)
  encryption/                Envelope encryption (AES-256-GCM, key providers)
  cache/                     Object data LRU cache with TTL
  lifecycle/                 Background service manager
  testutil/
    mock_store.go            Shared MetadataStore mock for integration tests
grafana/
  s3-orchestrator.json       Grafana dashboard (all Prometheus metrics)
sqlc.yaml                    sqlc configuration
Dockerfile                   Multi-stage build
Makefile                     Build, test, lint, generate, push, deb targets
nfpm.yaml                    Debian package definition (nfpm)
packaging/
  s3-orchestrator.service    Systemd unit file
  config.yaml                Sample config installed to /etc/s3-orchestrator/
  s3-orchestrator.default    Default env file installed to /etc/default/
  preinstall.sh              Creates system user and directories
  postinstall.sh             Enables systemd service
  postremove.sh              Purge cleanup (removes user and data)
  changelog                  Debian changelog
  copyright                  Debian copyright file
  lintian-overrides          Lintian override rules
cliff.toml                   git-cliff changelog generation config
CHANGELOG.md                 Auto-generated changelog (make changelog)
config.example.yaml          Configuration reference
deploy/
  nomad/
    s3-orchestrator.nomad.hcl  Production Nomad job (Vault integration)
    local/
      s3-orchestrator.nomad.hcl  Local dev job (docker-compose backing services)
      demo.sh                  One-command Nomad dev demo
  helm/
    s3-orchestrator/
      Chart.yaml               Helm chart metadata
      values.yaml              Default production values
      templates/               Deployment, Service, ConfigMap, Secret, Ingress, etc.
  kubernetes/
    local/
      values.yaml              Local dev Helm values (docker-compose backing services)
      demo.sh                  One-command k3d demo
```

## Additional Documentation

| Guide | Description |
|-------|-------------|
| [Quickstart](docs/quickstart.md) | Get running in under a minute |
| [User Guide](docs/user-guide.md) | S3 client configuration and usage |
| [Admin Guide](docs/admin-guide.md) | Configuration, operations, monitoring, deployment |
| [API Reference](docs/api-reference.md) | UI and Admin API JSON endpoint documentation |
| [Security Hardening](docs/security-hardening.md) | TLS, mTLS, config security, network segmentation |
| [Performance Tuning](docs/performance-tuning.md) | Connection pools, timeouts, routing, rebalancer tuning |
| [Disaster Recovery](docs/disaster-recovery.md) | Failure scenarios and recovery procedures |
| [Version Migration](docs/version-migration.md) | Upgrade guide, config changes by version |
| [Style Guide](docs/style-guide.md) | Coding conventions for contributors |
| [Contributing](CONTRIBUTING.md) | How to build, test, and submit changes |
