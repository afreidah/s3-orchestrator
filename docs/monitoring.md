---
title: "Monitoring"
linkTitle: "Monitoring"
weight: 31
---

The orchestrator emits three observability streams:

- **Prometheus metrics** — `/metrics` (95+ metrics under the `s3o_` prefix); see the [full reference table](#full-metric-reference) below.
- **OpenTelemetry traces** — every HTTP request, manager operation, and backend S3 call; OTLP-exported to Tempo or compatible collector.
- **Structured JSON logs** — `slog` to stdout, including a dedicated audit subset with `"audit": true`.

A ready-to-import Grafana dashboard covering every metric ships at `grafana/s3-orchestrator.json`.

### Web dashboard

When `ui.enabled` is `true`, the dashboard at `{path}/` shows a live snapshot of:

- **Storage summary** — total bytes used/capacity across all backends
- **Backend quota** — bytes used/limit with progress bars per backend, object counts, active multipart uploads
- **Integrity coverage** — how long ago the least recently verified copy was checked, and how many copies have never been checked. Shown when `integrity.enabled` is true. Distinct from the backend table's **No Hash** column, which counts copies with no stored hash at all: a fully hashed fleet can still be entirely unverified
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
- **Sync** - import pre-existing objects from a backend's S3 bucket into the proxy database. Select a backend and a virtual bucket - objects already in the database are skipped, and objects outside every configured virtual bucket prefix are imported as unmanaged so they count toward quota without the workers acting on them.

### On-demand reconciliation

When a backend loses data (expired credentials, provider outage, accidental deletion), the metadata database retains stale entries that cause log noise in the rebalancer, replicator, and scrubber. The reconcile endpoint walks both the backend (paginated `ListObjects`) and the metadata DB (paginated cursor over `object_locations` ordered by key) as ascending key streams, then merges them in lockstep. Memory is bounded by a 1000-entry page size on each side regardless of total object count, so a backend holding millions of objects reconciles without OOM. Every key found on the backend is imported at its literal key, including keys outside every configured virtual bucket prefix - those bytes occupy the backend's quota either way, so leaving them off the ledger makes space accounting wrong. Rows for such keys are marked unmanaged: they count toward quota, but replication, rebalance, integrity and drain ignore them.

```bash
# Reconcile all backends
curl -X POST -H "X-Admin-Token: $TOKEN" http://localhost:9000/admin/api/reconcile

# Reconcile a single backend
curl -X POST -H "X-Admin-Token: $TOKEN" http://localhost:9000/admin/api/reconcile?backend=g3
```

Response:
```json
{"status":"ok","imported":0,"removed":57,"backends_scanned":1}
```

- **imported**: objects on the backend but not in the DB (brought under management)
- **removed**: DB entries whose objects no longer exist on the backend (stale metadata cleaned up)
- **backends_scanned**: how many backends were processed

The background reconciler (`reconcile.enabled: true`) runs the same logic on a timer. The admin endpoint is for immediate use after incidents.

The dashboard requires authentication. Users log in at `{path}/login` with the `admin_key` and `admin_secret` configured in the `ui` section. Sessions last 24 hours.

The dashboard is server-rendered HTML. The object tree uses JavaScript for lazy-loaded directory expansion — directories fetch their children on click via the `/ui/api/tree` endpoint.

JSON endpoints at `{path}/api/dashboard`, `{path}/api/tree`, and `{path}/api/logs` return data for programmatic access or integration with other tools. The logs endpoint accepts optional query parameters: `level` (minimum severity: DEBUG, INFO, WARN, ERROR), `since` (RFC3339 timestamp), `component`, and `limit`. Management endpoints (`{path}/api/delete`, `{path}/api/upload`, `{path}/api/rebalance`, `{path}/api/clean-excess`, `{path}/api/sync`) accept POST requests. The download endpoint (`{path}/api/download?key=...`) accepts GET requests. All API endpoints require authentication.

### Health endpoints

Two health endpoints serve different purposes:

**Liveness** (`/health`) — always returns HTTP 200. Use this for liveness probes (Consul checks, K8s livenessProbe). The service stays in rotation during temporary database outages.

```bash
curl http://localhost:9000/health
# {"status":"ok"}
# or {"status":"degraded"} when the database circuit breaker is open
```

**Readiness** (`/health/ready`) — returns HTTP 200 when the service is ready to handle traffic, HTTP 503 during startup (before migrations and backend initialization complete) and during shutdown drain. Use this for readiness probes (K8s readinessProbe, Nomad `on_update = "require_healthy"`).

```bash
curl http://localhost:9000/health/ready
# {"status":"ready"}      — 200
# {"status":"not ready"}  — 503
```

The HTTP response body is intentionally minimal — only the `status` field is returned, so log aggregators can grep on a fixed pattern. To identify which instance answered a probe in a multi-instance deployment, query `GET /admin/api/workers` (which includes the instance identifier) or correlate by source IP.

### Background worker health

`/health` only reflects database breaker state. Background services
(replicator, cleanup queue, lifecycle, pending reaper, ...) are
supervised by the lifecycle manager and recover on their own, but a
service that is *running* yet *failing every tick* looks identical to
a healthy one in `/health`.

`GET /admin/api/workers` returns a JSON snapshot of every registered
background service's last-tick health, including `last_success`,
`last_failure`, `last_error`, and `consecutive_failures`. Use it
during incidents to distinguish:

```bash
curl -H "X-Admin-Token: $TOKEN" http://localhost:9000/admin/api/workers
```

```json
{
  "workers": [
    {"name": "cleanup_queue", "last_success": "2026-05-12T18:42:01Z", "consecutive_failures": 0},
    {"name": "replicator", "last_failure": "2026-05-12T18:45:14Z", "last_error": "connection refused", "consecutive_failures": 3}
  ]
}
```

Workers in proxy-only deployments return `503` from this endpoint
because no worker pool is registered.

The same data flows into Prometheus as
`s3o_worker_last_success_timestamp_seconds`,
`s3o_worker_consecutive_failures`, and `s3o_worker_ticks_total{result}`
so alerting can run without scraping the admin endpoint. Suggested
alert shapes are in the metrics table below.

### Grafana dashboard

A comprehensive Grafana dashboard is included at `grafana/s3-orchestrator.json`. Import it via Grafana's UI (Dashboards → Import → Upload JSON file) or provision it from disk. It expects a Prometheus datasource with UID `prometheus`.

The dashboard covers all emitted metrics, organised by domain into rows:
overview, quota & storage, request performance, backend operations,
manager operations, circuit breaker & degraded mode, replication, usage
tracking, rate limiting & rejections, rebalancer, drain & lifecycle,
cleanup queue & audit, encryption, object data cache, integrity
verification, Redis, over-replication cleanup, pending PUT intents,
and authentication (streaming SigV4). Rows for less frequently inspected
domains are collapsed by default.

### Key Prometheus metrics

If `telemetry.metrics.enabled` is `true`, metrics are exposed at `/metrics`. Two listener modes:

- **Inline (`telemetry.metrics.listen` empty, the default):** `/metrics` is served on the same listener as the public S3 API. Convenient for single-port deployments and the docker-compose / local-dev workflow. In this mode `/debug/pprof/*` endpoints are **not** registered — mounting pprof on the public S3 listener would leak runtime internals (de-anonymized stack frames via `/debug/pprof/heap`, the command line and flag values via `/debug/pprof/cmdline`) and offer a DoS amplifier (`/debug/pprof/profile?seconds=300` triggers minutes of CPU profiling on demand).
- **Dedicated listener (`telemetry.metrics.listen` set, e.g. `127.0.0.1:9001`):** `/metrics` and `/debug/pprof/*` are both mounted on the dedicated listener. Bind to `127.0.0.1` or a private network interface so the surface is only reachable from operators inside the trust boundary. The nomad demo uses `0.0.0.0:9001` so the docker-compose Prometheus container can scrape via the bridge gateway; production deployments should tighten this to `127.0.0.1:9001` or a private network address.

Once the dedicated listener is configured, captures look like:

```bash
# 60-second allocation profile during a load spike
curl -o /tmp/allocs.pb.gz "http://127.0.0.1:9001/debug/pprof/allocs?seconds=60"
go tool pprof -top -cum -alloc_space /tmp/allocs.pb.gz

# Instantaneous live-heap snapshot
curl -o /tmp/heap.pb.gz "http://127.0.0.1:9001/debug/pprof/heap"
go tool pprof -top -cum -inuse_space /tmp/heap.pb.gz
```

The standard `net/http/pprof` endpoints are available: `/debug/pprof/`, `/debug/pprof/profile`, `/debug/pprof/heap`, `/debug/pprof/allocs`, `/debug/pprof/goroutine`, `/debug/pprof/block`, `/debug/pprof/mutex`, `/debug/pprof/trace`, `/debug/pprof/cmdline`, `/debug/pprof/symbol`.

Key metrics to alert on:

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
| `s3o_over_replication_key_preserved_total` | Any non-zero value means a copy set disagrees about encryption and the cleaner kept the only copy that can still be decrypted. Repair the diverged rows; until then the object stays over-replicated |
| `s3o_encryption_flag_mismatch_total{component}` | Any non-zero value is an alert: an object's stored bytes disagree with its recorded encryption flag, so that copy cannot be read or hashed. `component` names what noticed (`get`, `head`, `scrubber`) |
| `s3o_import_classified_total{decision="unreadable"}` | Reconcile or sync found an encrypted object whose key is gone. It is recorded so its space is accounted for, but nothing can serve it. Restore it from elsewhere or delete it |
| `s3o_integrity_oldest_unverified_seconds` | Alert when it climbs steadily. The scrubber is not completing a sweep. It should settle around the sweep period implied by `scrubber_interval` and `scrubber_batch_size`; a rising value means the batch is too small or the interval too long for the fleet. A copy never verified at all counts from when it was written, so a fleet the sweep has not reached shows its true backlog rather than zero |
| `s3o_integrity_never_verified_copies` | Stays non-zero on a fleet being written to, since new copies queue behind older data by design. Backfill-checksums adds to it directly, since it gives objects a hash without verifying them. Alert on a climbing value you did not cause, not on it being above zero |
| `s3o_compression_errors_total{operation="decode"}` | Any non-zero rate is an alert: bytes already stored cannot be read back. An `encode` failure only fails the write that caused it, but a decode failure means an object is unreadable now |
| `s3o_compression_fetched_bytes_total / s3o_compression_served_bytes_total` | Read amplification. Bounded by chunk size relative to the ranges clients ask for, so it should be flat. A climb towards the average object size means reads have stopped being ranged, which is visible nowhere else except the backend bill |
| `s3o_requests_total{status_code="5xx"}` | Alert on elevated 5xx rates |
| `s3o_http_panic_recovered_total{route}` | Any non-zero rate is an alert: a handler panicked and the recovery middleware returned a 500. Pivot via the matching `http.PanicRecovered` audit entry for the captured stack and request id |
| `s3o_degraded_write_rejections_total` | Writes being rejected due to degraded mode |
| `s3o_usage_limit_rejections_total` | Operations rejected by usage limits |
| `s3o_rate_limit_rejections_total` | Requests rejected by per-IP rate limiting |
| `s3o_admission_rejections_total` | Requests rejected at the hard admission limit |
| `s3o_load_shed_total` | Requests probabilistically shed before the hard admission limit |
| `s3o_early_rejections_total` | Uploads rejected before body transmission (no backend capacity) |
| `s3o_cleanup_queue_depth` | Alert when consistently > 0 — orphaned objects are failing cleanup |
| `s3o_cleanup_queue_processed_total{status="exhausted"}` | Items that exceeded max retries — graduated to the DLQ |
| `s3o_cleanup_dlq_depth` | Alert when > 0 — at least one unrecoverable orphan needs operator action |
| `s3o_cleanup_dlq_enqueued_total{backend="..."}` | Rate of graduations per backend; one backend dominating means its delete path is broken |
| `s3o_cleanup_queue_stale_claims_recovered_total{backend="..."}` | Non-zero rate means a worker died mid-process or `cleanup_queue.claim_grace_period` is shorter than realistic worst-case row processing time |
| `s3o_cleanup_enqueue_failures_total{backend,reason,stage}` | Alert on any non-zero rate of `stage="enqueue"` — backend object exists with no `cleanup_queue` row (orphan-leak risk). Pivot via the matching `storage.OrphanEnqueueFailed` audit event for backend/key/size, then run `POST /admin/api/reconcile` after DB recovery |
| `s3o_audit_events_total{event="..."}` | Audit log volume by event type — useful for detecting unusual activity |
| `s3o_pending_intents_enqueued_total` | Rate of PUT intents inserted (write-path PUT-before-COMMIT pattern). Should track the PutObject rate closely; a sustained gap suggests the pending pattern is bypassed |
| `s3o_pending_intents_resolved_total{status}` | Pending intents resolved by status (`committed` = atomic commit happy path, `promoted` = reaper found backend object and promoted the intent, `dropped` = reaper found HEAD 404 and dropped the intent, `ambiguous` = HEAD failed for non-404 reasons, `already_resolved` = race). A sustained `ambiguous` rate means the reaper cannot reach the backend |
| `s3o_pending_intents_depth` | Current unresolved intents. Alert when consistently above the `batch_size` of the reaper — the reaper is not keeping up |
| `s3o_drain_active` | Count of in-flight backend drain operations (Inc/Dec so concurrent drains compose); 0 means no drains are running. Page on `s3o_drain_active > 0 for 6h` (drain stuck) |
| `s3o_drain_race_aborted_total` | PutObject attempts aborted after drain started mid-write. Any non-zero rate is benign (the orchestrator recovers automatically) but a sustained rate suggests longer-than-expected gaps between `EligibleForWrite` and the backend PUT — typically very large objects against a fast-draining backend |
| `time() - s3o_worker_last_success_timestamp_seconds{service="..."}` | Alert when greater than the worker's expected tick interval times a margin (e.g. `> 4 * interval`) — the service has not completed a successful tick in that window |
| `s3o_worker_consecutive_failures{service="..."}` | Alert when consistently > 0 — the service is running but every tick fails; logs and `/admin/api/workers` carry the underlying error |
| `rate(s3o_worker_ticks_total{result="error"}[15m])` | Persistent error rate; alongside `consecutive_failures` distinguishes flapping from sustained failure |
| `s3o_auth_streaming_requests_total{variant}` | Rate of streaming-payload SigV4 PUTs by variant — track which client SDKs are sending streaming uploads |
| `s3o_auth_streaming_rejections_total{reason}` | Alert on any non-zero rate — every increment is a chunk-validation failure (tampered body, malformed framing, length mismatch, or signature mismatch) |
| `s3o_encryption_errors_total{op,error_type}` | Any non-zero rate indicates encryption/decryption failures. `error_type="stream_failed"` specifically flags transport errors that surfaced mid-stream (after the encryptor/decryptor was constructed). |
| `s3o_encrypt_existing_objects_total{status="error"}` | Failures during bulk encryption of existing data |
| `s3o_decrypt_existing_objects_total{status="error"}` | Failures during bulk decryption of existing data |
| `s3o_key_rotation_objects_total{status="error"}` | Failures during key rotation |
| `s3o_redis_fallback_active` | Alert when 1 — Redis is unavailable, using local counters |
| `s3o_redis_operations_total{operation,status}` | Track Redis operation success/error rates |
| `s3o_cache_hits_total` / `s3o_cache_misses_total` | Cache hit ratio — low hit rate may indicate the cache is undersized or the workload is not read-heavy |
| `s3o_cache_evictions_total` | High eviction rate suggests `max_size` is too small for the working set |
| `s3o_cache_size_bytes` / `s3o_cache_entries` | Current cache utilization - watch for the cache staying near `max_size`. Also served, alongside the lifetime hit and miss counts, by `GET /admin/api/cache` for operators without Prometheus |

### Full metric reference

All metrics are prefixed with `s3o_`. Exposed at `/metrics` when `telemetry.metrics.enabled` is true.

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
| `s3o_rebalance_runs_total` | Counter | strategy, status | Rebalancer executions, by cycle outcome (see below) |
| `s3o_rebalance_duration_seconds` | Histogram | strategy | Rebalancer execution time |
| `s3o_rebalance_skipped_total` | Counter | reason | Rebalancer runs skipped |
| `s3o_rebalance_pending` | Gauge | — | Objects planned for rebalance |
| `s3o_replication_pending` | Gauge | — | Objects below replication factor |
| `s3o_replication_copies_created_total` | Counter | — | Replica copies created |
| `s3o_replication_errors_total` | Counter | — | Replication errors |
| `s3o_replication_duration_seconds` | Histogram | — | Replication cycle time |
| `s3o_replication_runs_total` | Counter | status | Replication worker executions, by cycle outcome (see below) |
| `s3o_replication_health_copies_total` | Counter | — | Copies created to replace copies on circuit-broken backends |
| `s3o_over_replication_pending` | Gauge | — | Objects exceeding the replication factor |
| `s3o_over_replication_removed_total` | Counter | — | Excess copies removed |
| `s3o_over_replication_errors_total` | Counter | — | Over-replication cleanup errors |
| `s3o_over_replication_key_preserved_total` | Counter | — | Copies kept because they held the only usable encryption key for the object |
| `s3o_over_replication_runs_total` | Counter | status | Over-replication worker executions, by cycle outcome (see below) |
| `s3o_over_replication_duration_seconds` | Histogram | — | Over-replication cleanup cycle time |
| `s3o_circuit_breaker_state` | Gauge | name | 0=closed, 1=open, 2=half-open (name: "database" or backend name) |
| `s3o_circuit_breaker_transitions_total` | Counter | name, from, to | State transitions per component |
| `s3o_circuit_breaker_internal_errors_total` | Counter | — | Errors returned by breaker post-check or state transitions |
| `s3o_degraded_mode_active` | Gauge | — | 1 while the read path is in degraded mode (database unavailable), 0 otherwise |
| `s3o_degraded_reads_total` | Counter | operation | Broadcast reads in degraded mode |
| `s3o_degraded_cache_hits_total` | Counter | — | Cache hits during degraded reads |
| `s3o_degraded_write_rejections_total` | Counter | operation | Writes rejected in degraded mode |
| `s3o_degraded_broadcast_duration_seconds` | Histogram | outcome | Wall-clock duration of degraded-mode broadcast reads, by terminal outcome |
| `s3o_degraded_broadcast_drain_timeout_total` | Counter | — | Loser-drain goroutines that gave up at the bounded timeout because a cancelled probe never returned. Non-zero means a backend is stranding goroutines under degraded fan-out |
| `s3o_degraded_broadcast_mixed_outcomes_total` | Counter | — | Broadcasts whose all-failed terminal saw both 404 and non-404 failures, which surfaces provider divergence otherwise hidden under not_found |
| `s3o_write_failover_total` | Counter | — | Write operations that failed over to a different backend |
| `s3o_quota_reconcile_corrections_total` | Counter | backend | Per-backend `bytes_used` drift corrections applied by usage reconciliation |
| `s3o_usage_api_requests` | Gauge | backend | Current month API request count |
| `s3o_usage_egress_bytes` | Gauge | backend | Current month egress bytes |
| `s3o_usage_ingress_bytes` | Gauge | backend | Current month ingress bytes |
| `s3o_usage_limit_rejections_total` | Counter | operation, limit_type | Operations rejected by usage limits. `operation` covers the client paths plus `rebalance` (moves not planned) and `scrub` (copies deferred) |
| `s3o_cleanup_queue_enqueued_total` | Counter | reason | Items added to the cleanup retry queue |
| `s3o_cleanup_queue_processed_total` | Counter | status | Items processed from the cleanup queue (success/success_absent/retry/exhausted) |
| `s3o_cleanup_queue_depth` | Gauge | — | Current pending items in the cleanup queue |
| `s3o_cleanup_queue_stale_claims_recovered_total` | Counter | backend | Cleanup_queue rows whose stale claim was reclaimed by a later worker tick |
| `s3o_cleanup_dlq_depth` | Gauge | — | Unrecoverable orphans waiting in the cleanup dead-letter table |
| `s3o_cleanup_dlq_enqueued_total` | Counter | backend | Cleanup rows graduated to the dead-letter after exhausting retries |
| `s3o_cleanup_enqueue_failures_total` | Counter | backend, reason, stage | Cleanup-queue enqueue attempts that failed after a successful backend write (orphan risk) |
| `s3o_pending_intents_enqueued_total` | Counter | — | In-flight PUT intents inserted before the backend write |
| `s3o_pending_intents_resolved_total` | Counter | status | Pending PUT intents resolved by status (committed, promoted, dropped, ambiguous, already_resolved) |
| `s3o_pending_intents_depth` | Gauge | — | Current number of unresolved pending PUT intents |
| `s3o_rate_limit_rejections_total` | Counter | — | Requests rejected by per-IP rate limiting |
| `s3o_admission_rejections_total` | Counter | — | Requests rejected by server-level admission control |
| `s3o_admission_client_canceled_total` | Counter | — | Admission waits abandoned because the client cancelled first |
| `s3o_worker_admission_rejections_total` | Counter | worker | Background worker tasks skipped because admission control was saturated |
| `s3o_lifecycle_deleted_total` | Counter | — | Objects deleted by lifecycle expiration |
| `s3o_lifecycle_failed_total` | Counter | — | Objects that failed lifecycle deletion |
| `s3o_lifecycle_runs_total` | Counter | status | Lifecycle worker executions |
| `s3o_worker_ticks_total` | Counter | service, result | Locked-ticker service ticks by service and outcome |
| `s3o_worker_last_success_timestamp_seconds` | Gauge | service | Unix time of the most recent successful tick per service |
| `s3o_worker_consecutive_failures` | Gauge | service | Consecutive failed ticks per service since the last success |
| `s3o_audit_events_total` | Counter | event | Audit log entries emitted |
| `s3o_drain_active` | Gauge | — | Count of in-flight backend drain operations |
| `s3o_drain_objects_moved_total` | Counter | — | Objects migrated during drain |
| `s3o_drain_bytes_moved_total` | Counter | — | Bytes migrated during drain |
| `s3o_drain_race_aborted_total` | Counter | — | PutObject attempts aborted after drain started mid-write |
| `s3o_encryption_operations_total` | Counter | op | Encrypt/decrypt operations |
| `s3o_encryption_errors_total` | Counter | op, error_type | Encryption/decryption failures |
| `s3o_encryption_unknown_key_id_total` | Counter | — | Decryption attempts with unknown keyID (primary key fallback) |
| `s3o_encryption_flag_mismatch_total` | Counter | component | Copies whose stored bytes disagreed with their recorded encryption flag |
| `s3o_import_classified_total` | Counter | source, decision | Discovered objects imported, by what their bytes were classified as (`plaintext`, `adopted_key`, `unreadable`, `compressed`) |
| `s3o_encrypt_existing_objects_total` | Counter | status | Objects processed by encrypt-existing |
| `s3o_compress_existing_objects_total` | Counter | status | Objects processed by compress-existing. `status="skipped"` counts objects deliberately left alone, which is the expected outcome for media and archives |
| `s3o_decompress_existing_objects_total` | Counter | status | Objects processed by decompress-existing |
| `s3o_bulk_rewrite_usage_declined_total` | Counter | operation | Objects a rewrite pass skipped because the backend had no usage headroom for the read and write. Distinct from a deliberate skip: this work was wanted and did not happen, so a pass reporting these has not finished its fleet |
| `s3o_compression_logical_bytes_total` | Counter | — | Bytes clients wrote for objects that were then stored compressed |
| `s3o_compression_stored_bytes_total` | Counter | — | What those objects occupy after encoding. Divide by the above for the fleet's ratio; subtract for bytes saved |
| `s3o_compression_ratio` | Histogram | — | Encoded size as a fraction of logical size, per object |
| `s3o_compression_fetched_bytes_total` | Counter | — | Stored bytes fetched from backends while serving compressed objects |
| `s3o_compression_served_bytes_total` | Counter | — | Logical bytes served to clients from those reads. Divide the above by this for read amplification |
| `s3o_compression_skipped_total` | Counter | reason | Objects stored verbatim despite compression being on (`min_size`, `min_ratio`) |
| `s3o_compression_errors_total` | Counter | operation | Codec failures (`encode`, `decode`) |
| `s3o_encryption_plaintext_copies` | Gauge | — | Copies still stored unencrypted; falls only when encrypt-existing is run |
| `s3o_decrypt_existing_objects_total` | Counter | status | Objects processed by decrypt-existing |
| `s3o_key_rotation_objects_total` | Counter | status | DEKs re-wrapped by key rotation |
| `s3o_redis_operations_total` | Counter | operation, status | Redis command outcomes |
| `s3o_redis_fallback_active` | Gauge | — | `1` when Redis is unavailable and using local counters |
| `s3o_cache_hits_total` | Counter | — | Object data cache hits |
| `s3o_cache_misses_total` | Counter | — | Object data cache misses |
| `s3o_cache_evictions_total` | Counter | — | Object data cache evictions (LRU or TTL) |
| `s3o_cache_size_bytes` | Gauge | — | Current memory used by cached objects |
| `s3o_cache_entries` | Gauge | — | Current number of cached objects |
| `s3o_head_served_from_metadata_total` | Counter | — | HEAD responses answered from the object ledger, each one a backend request and a metered API call avoided |
| `s3o_cache_flush_total` | Counter | — | Admin-triggered object data cache flushes |
| `s3o_cache_admin_invalidations_total` | Counter | — | Admin-triggered single-key cache invalidations |
| `s3o_integrity_checks_total` | Counter | operation | Integrity hash verifications performed (read, scrub) |
| `s3o_integrity_errors_total` | Counter | operation | Hash mismatches detected (corrupted copies enqueued for cleanup) |
| `s3o_integrity_oldest_unverified_seconds` | Gauge | — | How long the most overdue object copy has gone unverified |
| `s3o_integrity_never_verified_copies` | Gauge | — | Copies with a content hash that have never been verified |
| `s3o_auth_streaming_requests_total` | Counter | variant | Streaming-payload SigV4 PUTs by variant |
| `s3o_auth_streaming_rejections_total` | Counter | reason | Chunk-validation failures (tampered body, signature mismatch, etc.) |
| `s3o_notification_queue_depth` | Gauge | — | Pending webhook events queued for delivery |
| `s3o_notification_sent_total` | Counter | endpoint, event_type | Successfully POSTed webhook events |
| `s3o_notification_failed_total` | Counter | endpoint, event_type | Webhook POST failures (before retry) |
| `s3o_notification_dropped_total` | Counter | endpoint, event_type | Webhook events dropped (dampened, or the outbox enqueue failed) |
| `s3o_notification_duration_seconds` | Histogram | endpoint | Webhook delivery latency |
| `s3o_notification_store_errors_total` | Counter | — | Outbox persistence failures |
| `s3o_http_panic_recovered_total` | Counter | route | Handler panics caught by the recovery middleware |

Quota metrics are refreshed from PostgreSQL every 30 seconds (no backend API calls).

#### Worker cycle outcomes

The `*_runs_total` counters for the rebalance, replication and over-replication workers carry a `status` label describing how the cycle finished, taken from the per-item tally rather than from the fact that the cycle returned:

| Value | Meaning |
|---|---|
| `success` | Every item the cycle attempted succeeded |
| `partial` | Some items succeeded and some failed |
| `failed` | Every item the cycle attempted failed |
| `empty` | The cycle ran with nothing to do |
| `error` | The cycle could not start: the query that selects the batch failed |

Alert on `partial` and `failed` to catch a worker that is running on schedule but not achieving anything - a backend that rejects every write, for instance, produces `failed` cycles indefinitely while the process stays healthy. `empty` is the normal steady state for a fleet that is already in shape.

### OpenTelemetry tracing

Spans are emitted for every HTTP request, manager operation, and backend S3 call. The service registers as `s3-orchestrator` (`resource.service.name`). Traces propagate via W3C `traceparent` headers. Configured to export via gRPC OTLP to Tempo or any OTLP-compatible collector.

**Trace-to-log correlation** — every JSON log line emitted within an active span automatically includes `trace_id` and `span_id` fields. Log aggregators (Loki, etc.) can use these fields to link logs to their corresponding traces in Tempo or any OpenTelemetry-compatible tracing backend. Only log calls that receive a `context.Context` with an active span include trace context; application-level logs without a span context are unaffected.

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
| `cleanup_queue.already_absent` | Cleanup queue | Backend DELETE returned 404 — the object is already gone. Row dropped as idempotent success instead of retrying nine more times |
| `cleanup_queue.claim_recovered` | Cleanup queue | A row whose claim aged past the grace period was reclaimed by a different worker tick (typical after a process crash or rolling-deploy overlap) |
| `cleanup_queue.exhausted_to_dlq` | Cleanup queue | Row graduated to `cleanup_dlq` after exhausting retries |
| `over_replication.start`, `over_replication.complete`, `over_replication.remove` | Over-replication cleaner | Surplus replica removal cycle; `.remove` carries the per-copy decision |
| `pending_reaper.promoted` | Pending reaper | The reaper HEADed the backend, found the object, and promoted the pending intent into a committed `object_locations` row |
| `pending_reaper.dropped` | Pending reaper | The reaper HEADed the backend and got 404. No orphan exists; the intent row is dropped |
| `pending_reaper.superseded` | Pending reaper | A later write for the same key completed and superseded the pending intent before the reaper resolved it |
| `storage.OrphanEnqueueFailed` | Coordinator | The cleanup-queue enqueue path itself failed after a successful backend write (DB outage). Carries backend / key / size / stage so an operator can reconcile manually once DB connectivity returns |
| `storage.UploadPart` | Multipart | A multipart part upload completed successfully |
| `http.PanicRecovered` | HTTP panic-recovery middleware | A handler panicked and the recovery layer returned a 500 to the client. Carries `route`, `method`, `path`, and the panic value. The matching error-level slog entry carries the captured stack trace |

Each S3 API request produces two correlated audit entries (HTTP-level and storage-level) sharing the same `request_id`. Internal operations (rebalance, replication) generate their own correlation IDs. The `request_id` also appears as a `s3o.request_id` attribute on OpenTelemetry spans.

### HTTP panic recovery

A panic inside an HTTP handler is caught by the panic-recovery middleware applied to the S3 and admin route groups. The recovery contract:

- The client gets a structured response, not a connection reset. S3 routes receive an XML `<Error><Code>InternalError</Code><Message>...Request ID: ...</Message></Error>` body with HTTP 500; admin routes receive the same shape as a JSON `{"error": "...Request ID: ..."}`.
- The Prometheus counter `s3o_http_panic_recovered_total{route}` increments. A non-zero value is an immediate alert candidate.
- A `slog.ErrorContext` line is written at level `ERROR` with `component=httputil`, the route, the method and path, the panic value, the captured Go stack, and a `trace_id` / `span_id` if a span was active when the panic occurred.
- An `http.PanicRecovered` audit entry is emitted with the same correlation `request_id` as the failing request.
- If an OpenTelemetry span was active on the request, it is marked as failed via `SetStatus(Error)` + `RecordError` so traces in Tempo highlight the failure.

UI routes are intentionally not wrapped in the first iteration (mounted on the same mux as the S3 catch-all; the bulk of panic risk is on S3 and admin anyway). The recovery message deliberately does not echo the panic value to the client; only the request ID is returned so support tickets can be correlated back to the orchestrator log line.

Clients can supply their own correlation ID via the `X-Request-Id` request header; otherwise the orchestrator generates one. The ID is returned in the `X-Amz-Request-Id` response header.

**Trace-to-log correlation** — JSON log output includes `trace_id` and `span_id` fields on every line emitted within an active OpenTelemetry span. Log aggregators like Grafana Loki can extract these fields to link directly from a log entry to the corresponding trace in Tempo, and vice versa.
