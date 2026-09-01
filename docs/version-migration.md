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
# s3-orchestrator v0.57.1 go1.26.3 linux/amd64
```

## Compatibility Matrix

| Component | Tested Versions | Compatible | Notes |
|-----------|----------------|------------|-------|
| PostgreSQL | 16, 18 | 10+ (pgx driver) | Required. Connection pooling via pgxpool. |
| Redis | 7, 8 | 6+ (go-redis driver) | Optional. Shared usage counters for multi-instance deployments. |
| Go | 1.27 | 1.27+ | For building from source. Version set in `go.mod`. |
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

### v0.117.x (current)

**`Last-Modified` is now the object's write time, not the serving copy's ([#1356](https://github.com/afreidah/s3-orchestrator/issues/1356), v0.117.0)**

An object's `Last-Modified` used to depend on which copy answered the request. A GET reported the serving backend's own modification time, so failing over to a replica changed it; a listing reported `MIN(created_at)` across copies, which was a third value again. Nothing about the object had changed in either case. Conditional requests are built on this header, so `If-Modified-Since` and `If-Range` were comparing against a value that moved on its own.

Three things changed. Replication now carries the source row's `created_at` to the new copy instead of stamping the moment the copy was made. Reconcile and `sync` record the modification time the backend reported for a discovered object, falling back to the moment of discovery when the backend reports none. Reads now prefer the stored timestamp over the backend's, which is the same precedence [#1340](https://github.com/afreidah/s3-orchestrator/issues/1340) established for ETag.

This also removes the failure mode that the fallback was originally added for ([#1182](https://github.com/afreidah/s3-orchestrator/issues/1182)): a backend that omits a modification time on GET can no longer produce an empty `Last-Modified`, because the stored value is always present and is now consulted first.

Fixing the write paths alone would not have repaired anything already stored, because a read answers from whichever copy served it: an object replicated under an older version would keep reporting a different time per copy, just sourced from the ledger instead of the backend. Migration `00022` therefore aligns existing rows, setting every copy of a key to `MIN(created_at)` across its copies. That is the correct target rather than an arbitrary pick - the copy the client's own write created is the one stamped at the write, and every later value belongs to a replica.

**Operator action items after upgrade:**

- No config change required. The backfill runs automatically as part of the usual startup migration and needs no operator step.
- **The backfill only moves timestamps backwards.** Lifecycle expiry reads the same column, so an object whose replicas were carrying a later stamp now ages from its real write time. That is the intended behaviour, but it can put an object closer to expiry than its pre-migration value suggested. If you run lifecycle rules with short windows, review `expiration_days` against the objects in your fleet before upgrading.
- The migration is not reversible. The per-copy stamps it replaces are not recoverable, and its `Down` is a no-op.
- Objects discovered by reconcile before this version were stamped at discovery time rather than their backend modification time. The backfill aligns their copies with each other but cannot recover the original write time; re-running reconcile will not correct them either, since the rows already exist and import leaves existing rows alone.
- `created_at` is now the object's write time on every copy, so it is no longer a per-copy age. The scrub queue was already using `last_scrubbed_at` for that and is unaffected.

### v0.101.x

**`integrity.verify_on_replicate` now does something ([#1292](https://github.com/afreidah/s3-orchestrator/issues/1292), v0.101.0)**

The field has been parsed and documented since v0.41.x, but no code read it, so replicas were never hash-checked whatever it was set to. It is now wired into the replicator: when enabled, each new copy is read back from its target, decoded to plaintext, and compared against the source's `content_hash` before the ledger row that makes it count toward the replication factor is written. A copy whose hash disagrees is deleted and another target is tried.

**Default changed from `true` to `false`.** The documented default was never in force, so no deployment's behaviour changes on upgrade. It is off because it is the one integrity check that is not close to free: reading each new copy back means a replica costs its size in egress twice. Every other check in this section is opt-in for the same reason, and hashing on write stays on because it rides a buffering pass the write path already performs.

**Operator action items after upgrade:**

- No config change required. A config that already sets `verify_on_replicate: true` starts verifying replicas on upgrade - expect replication egress on the receiving backends to roughly double. Remove the field or set it to `false` to keep the previous behaviour.
- If you enable it, run `admin backfill-checksums` first. A source with no `content_hash` has nothing to compare against, and its replicas are recorded unverified.
- `s3o_integrity_checks_total{operation="replicate"}` and `s3o_integrity_errors_total{operation="replicate"}` report the new check. A non-zero error rate does not say which end is damaged: the copy is byte-identical to its source, so a source that has already rotted makes every target disagree. Both backend names are logged; `admin scrub -key <key>` settles it.

### v0.76.x

**Reconcile imports objects at their literal key ([#1161](https://github.com/afreidah/s3-orchestrator/pull/1161), v0.76.0)**

Reconcile previously rewrote every backend key that matched no configured virtual bucket prefix, prepending the pass's bucket prefix before comparing it against the ledger. Backends list in byte order, so rewriting only some keys made the stream non-monotonic and broke the sorted-merge: on each pass the merge deleted a block of `object_locations` rows and then re-imported them, reporting a large import and removal count every 24 hours against a backend that had not changed. Because `ImportObject` writes no `content_hash`, the integrity backfill also never stayed done.

Objects are now imported at their literal backend key. Keys outside every configured virtual bucket prefix are still imported - they are real bytes against the backend's quota, and omitting them makes space accounting wrong - but their `object_locations` row is marked unmanaged via the new `managed` column (migration `00014_object_managed_flag`, default true). Quota sums every row; replication, rebalance, integrity and drain consider only managed ones.

The reconcile stream now also rejects a non-ascending backend listing or ledger cursor outright rather than silently mis-merging.

**Operator action items after upgrade:**

- Expect one larger-than-usual reconcile pass on backends that hold objects outside the configured virtual bucket prefixes: those objects are imported once, and `backend_quotas.bytes_used` rises to include them. That new total is the correct one - the earlier figure undercounted real consumption.
- Repeating import/remove counts on an otherwise idle backend should stop. A backend still reporting them after this upgrade is a genuine drift signal.
- No config change.

### v0.64.x

**Terminal object browser (`tui`) + object-locations response reshape ([#1085](https://github.com/afreidah/s3-orchestrator/pull/1085), v0.64.0)**

A new read-only `s3-orchestrator tui` subcommand launches a full-screen terminal object browser with an inspector pane that shows every backend copy of an object. See [CLI subcommands](cli.md#tui).

As part of the same change, the `GET /admin/api/object-locations` response was retyped. Each `locations[]` entry now uses snake_case fields (`backend`, `size_bytes`, `created_at`, `encrypted`, `key_id`, `plaintext_size`, `content_hash`) instead of the previous PascalCase shape (`BackendName`, `SizeBytes`, `CreatedAt`), and the per-copy `ObjectKey` field was dropped - the key is already the top-level `key`.

**Security fix:** the response previously included each copy's wrapped envelope encryption key, serialized on every object-locations request and printed by `admin object-locations`. That field has been removed - the endpoint now exposes encryption metadata only (`encrypted`, `key_id`).

**Operator action items after upgrade:**

- Any tooling that parses `admin object-locations` output (or the raw endpoint) must read the new snake_case field names; `BackendName`/`SizeBytes`/`CreatedAt` and the per-copy `ObjectKey` are gone.
- If object-locations responses were exposed to untrusted parties on a prior version, treat the affected copies' wrapped data-encryption keys as disclosed. They are only usable by someone who also holds the master key, but re-wrapping them via `admin rotate-encryption-key` restores confidentiality where that assumption may not hold.
- No config change.

### v0.57.x

**Cleanup DELETE 404 treated as idempotent success ([#877](https://github.com/afreidah/s3-orchestrator/pull/877), v0.57.1)**

A backend cleanup `DeleteObject` that returns HTTP 404 / `NoSuchKey` is now treated as idempotent success: the `cleanup_queue` row drops immediately instead of retrying nine more times and graduating to `cleanup_dlq`. The motivating incident: 63 phantom rows accumulated in `cleanup_dlq` against one MinIO backend over two days — all `StatusCode: 404`, none of them real un-cleanable orphans (the upstream PUTs had silently failed during a network outage, so the cleanup correctly identified objects to remove and got confused when the backend already agreed they didn't exist). The DLQ noise masked real un-cleanable orphans.

The same 404 → drop logic was also added to `DeleteOrEnqueue` in the write coordinator, so a 404 never seeds the cleanup queue in the first place.

New surfaces:

- `s3o_cleanup_queue_processed_total{status="success_absent"}` — counter label for idempotent drops. Add this to dashboards alongside `success` / `retry` / `exhausted`.
- `cleanup_queue.already_absent` audit event — emitted when the 404 path fires.

`backend.IsNotFound` was promoted from a private worker helper to an exported function in `internal/backend/`. Existing call sites in `worker/replicator.go`, `worker/pending.go`, and `backend/circuitbreaker.go` were consolidated onto it.

**Operator action items after upgrade:**

- Update any dashboard panels that filter on `s3o_cleanup_queue_processed_total{status=~"success|retry|exhausted"}` to include `success_absent`.
- No config change.

**Close drain-race + fix `s3o_drain_active` semantics + bound purge loop ([#876](https://github.com/afreidah/s3-orchestrator/pull/876), v0.57.0)**

Three related fixes around backend drain:

1. **Drain race closed.** A drain that began while a backend `PutObject` was in flight could land bytes on the now-draining backend. The write path now re-checks `IsDraining(backend)` after the backend PUT succeeds, before the metadata commit. On a positive re-check the bytes are cleaned up via `RecoverFromRecordFailure` and the attempt fails over to the next eligible backend.
2. **`s3o_drain_active` is now Inc/Dec instead of Set(1)/Set(0).** Concurrent drains across multiple backends now compose correctly — the gauge reports the count of in-flight drains, not just "is any drain running?".
3. **`PurgeBackendObjects` bails on zero DB progress.** A pathological list-and-fail loop (e.g., the page-list works but every per-row `DeleteObjectLocation` fails) could spin indefinitely. The worker now exits the page when it finishes a list with zero rows actually deleted, preventing the infinite-list-and-fail spin.

New surfaces:

- `s3o_drain_race_aborted_total` (counter) — increments each time the post-PUT re-check fires.

**Operator action items after upgrade:**

- Dashboards / alerts that read `s3o_drain_active == 1` should switch to `s3o_drain_active > 0` to keep working when multiple drains overlap.
- Add an alert on any non-zero rate of `s3o_drain_race_aborted_total`. A persistent rate suggests a longer-than-expected gap between `EligibleForWrite` and the backend PUT (e.g., very large objects against a fast-draining backend).

### v0.55.x – v0.56.x

**UsageTracker swapped to atomic.Pointer snapshots ([#874](https://github.com/afreidah/s3-orchestrator/pull/874), v0.56.0)**

The internal `UsageTracker` (the per-backend rolling-window counter feeding `BackendsWithinLimits` and the eligibility filter) replaced its `sync.RWMutex` pair with `atomic.Pointer[T]` snapshots and copy-on-write writes. The hot read path no longer touches a mutex.

No behavior change. Measured improvement on parallel `WithinLimits` benchmarks: 65.93 ns/op → 29.80 ns/op (~2.2× under contention). The change only matters at high request rates where `BackendsWithinLimits` is dispatched per request; below ~500 RPS it is in the noise.

**Operator action items:** none.

### v0.53.x – v0.54.x

**Optimize PutObject buffering + integrity pipeline ([#869](https://github.com/afreidah/s3-orchestrator/pull/869), v0.54.0)**

The `PutObject` body materialization layer (the buffer that lets the write path retry against a different backend on PUT failure) now spills to a tempfile above a configurable in-memory ceiling instead of always materializing to a `bytes.Buffer`. Combined with a heap-profile-friendly pipeline restructuring, container memory under sustained PUT load is significantly lower for workloads dominated by medium / large objects.

**Operator action items after upgrade:**

- Container memory limits sized off pre-v0.54 baselines can be tightened; equivalently, the previous limits absorb more concurrent in-flight PUTs.
- Tempfiles are written under the orchestrator's `TMPDIR` (defaults to `/tmp`). Operators running with a tmpfs `/tmp` should size it to accommodate `max_concurrent_writes × p99_object_size`, or set `TMPDIR` to a disk-backed location.

**Same-backend server-side copy fast path ([#868](https://github.com/afreidah/s3-orchestrator/pull/868), v0.53.0)**

`CopyObject` requests whose source and destination resolve to the same backend now dispatch through the backend's native `CopyObject` API (S3 `UploadPartCopy` / equivalent) instead of materializing through the orchestrator. This avoids one full GET + one full PUT per copy when the routing target matches the source backend.

New surfaces:

- Span attribute `s3o.native_copy=true` on the `CopyObject` span when the fast path is taken. Useful for trace filtering and for confirming the fast path actually fires in production.
- Per-backend accounting: the fast path records 1 API call against the backend and no egress/ingress (the bytes never traverse the orchestrator). Dashboards that derived "bytes copied" from `egress + ingress` will see a discontinuity if their workload is copy-heavy on the same backend.

**Operator action items after upgrade:**

- If you compute "data transferred" from `s3o_usage_egress_bytes` + `s3o_usage_ingress_bytes`, native-copy traffic is now invisible to that metric (which is correct — no bytes left the backend). Add a panel querying for spans with `s3o.native_copy=true` if you need a copy-volume signal.

### v0.51.x – v0.52.x

**Per-operation completion observability centralized ([#866](https://github.com/afreidah/s3-orchestrator/pull/866), v0.52.0)**

The audit / metric / span completion logic for `PutObject`, `GetObject`, `DeleteObject`, `HeadObject`, and the multipart operations was consolidated into a single `observe.Complete*` helper per operation. The behavior is unchanged for the existing audit events, but `storage.UploadPart` is now emitted on every successful part upload (previously only `CompleteMultipartUpload` emitted an event). This makes per-part backend distribution visible in audit logs.

**Operator action items after upgrade:**

- Audit log sinks that filter on `event=storage.UploadPart` will start seeing one entry per part. For a multipart of N parts, expect roughly N new entries per upload.

**Cancel losing degraded-read probes ([#867](https://github.com/afreidah/s3-orchestrator/pull/867), v0.52.1)**

When the read path is in degraded mode (one source unhealthy) it fires probe reads against multiple backends and serves the first one back. The losing probes were previously left to run to completion, wasting backend API calls and egress against quotas. They are now cancelled the moment a winner is declared. The visible effect is a drop in `s3o_usage_api_requests{backend=...}` during degraded operation, with no change to correctness or latency.

**Operator action items:** none.

### v0.49.x – v0.50.x

**Consumer-declared interfaces for proxy subpackages ([#847](https://github.com/afreidah/s3-orchestrator/pull/847), v0.49.0)**

The proxy package was split into subpackages (`object`, `multipart`, `readpath`, `writepath`, `accounting`, etc.), each declaring its own narrow consumer interface against `*infra.BackendRuntime` and the metadata store rather than importing the root `proxy` package. Internally-significant refactor.

Operator-visible: structured-log entries that used `component=backend_manager` now use `component=object`, `component=multipart`, or `component=writepath` depending on the subsystem doing the logging.

**Operator action items after upgrade:**

- Log-search saved queries that filter on `component=backend_manager` should add the new component names (`object`, `multipart`, `writepath`, `readpath`).
- Grafana log panels keyed on `component` will gain new series; old series will go quiet but not break.

### v0.48.x

Internal refactor only (proxy package decomposed into focused subpackages, [#845](https://github.com/afreidah/s3-orchestrator/pull/845)). No operator-visible behavior change. No action required.

### v0.47.x

**Surface orphan-enqueue failures during DB outages ([#824](https://github.com/afreidah/s3-orchestrator/pull/824), v0.47.5)**

When the write path enqueues a cleanup row after a partial write failure and the enqueue itself fails (e.g., the DB is unreachable), the orphan bytes were previously silent — the backend held data the orchestrator could not see. The failure path now emits a metric and an audit event so operators can pivot to the exact backend / key / size and reconcile manually once DB connectivity returns.

New surfaces:

- `s3o_cleanup_enqueue_failures_total{backend, reason, stage}` counter. `stage="enqueue"` means the cleanup_queue row itself did not persist (worst case — the cleanup worker will never see this orphan). `stage="orphan_bytes"` means the row persisted but the `orphan_bytes` counter did not increment (quota accounting drifts but cleanup still runs).
- `storage.OrphanEnqueueFailed` audit event carrying backend, key, size, stage, error.

**Operator action items after upgrade:**

- Alert on any non-zero rate of `s3o_cleanup_enqueue_failures_total{stage="enqueue"}` and run `POST /admin/api/reconcile` once DB connectivity returns to recover untracked orphans. See the admin guide cleanup runbook for the full procedure.

**Background worker health surfaced through admin API and Prometheus (v0.47.0)**

Every locked-ticker background service (replicator, cleanup queue,
rebalancer, lifecycle, pending reaper, over-replication cleaner,
scrubber, reconciler, multipart cleanup) now records per-tick
success/failure state. Operators can identify stalled or repeatedly
failing workers without scraping logs.

New surfaces:

- `GET /admin/api/workers` returns a JSON snapshot of every registered
  service's last success, last failure, last error, and consecutive
  failure count. Returns 503 in proxy-only deployments.
- Prometheus metrics: `s3o_worker_ticks_total{service,result}`,
  `s3o_worker_last_success_timestamp_seconds{service}`,
  `s3o_worker_consecutive_failures{service}`. The first labels every
  tick outcome (`success` / `error` / `skipped`); the second feeds
  staleness alerts; the third surfaces "running but failing".

**Operator action items after upgrade:**

- No configuration change. The endpoint and metrics are emitted
  automatically by every locked-ticker service.
- Consider adding Prometheus alerts on
  `time() - s3o_worker_last_success_timestamp_seconds{service="..."}`
  per critical worker. The existing supervisor still restarts crashed
  services; these alerts catch the harder case of "running but every
  tick fails".

### v0.46.x

**Postgres encrypt/decrypt admin keeps bytes_used consistent (v0.46.9)**

The Postgres `MarkObjectEncrypted` and `MarkObjectDecrypted` paths
updated `object_locations.size_bytes` but skipped the matching
`backend_quotas.bytes_used` adjustment. The bulk `encrypt-existing` and
`decrypt-existing` admin endpoints rewrite every object at a different
on-disk size (encryption adds per-chunk overhead, decryption removes
it), so after a bulk run on a Postgres deployment `bytes_used` drifted
permanently from `SUM(object_locations.size_bytes)`. The drift was
silent: write-routing trusted an under-counted `bytes_used` and the
backend silently overcommitted. The SQLite engine was correct.

The fix wraps both methods in a transaction that updates
`object_locations` and applies the size delta to
`backend_quotas.bytes_used` via a new `AdjustBackendBytesUsed` SQL.
`MarkObjectDecrypted` reads the current row inside the same
transaction so the delta is computed against the ciphertext size
about to be overwritten.

**Operator action items after upgrade:**

- If a Postgres deployment previously ran `encrypt-existing` or
  `decrypt-existing` and `backend_quotas.bytes_used` no longer matches
  `SUM(object_locations.size_bytes)`, run a one-time reconciliation:

  ```sql
  UPDATE backend_quotas bq
  SET bytes_used = COALESCE((
      SELECT SUM(size_bytes) FROM object_locations
      WHERE backend_name = bq.backend_name
  ), 0),
  updated_at = NOW();
  ```

  Run during a maintenance window: write-routing reads `bytes_used`
  and a stale value can briefly under-report or over-report capacity
  while this UPDATE is in flight.

- After the upgrade, future `encrypt-existing` / `decrypt-existing`
  runs do not need any manual reconciliation.

**Redis counter circuit breaker recovers cleanly without process restart (v0.46.8)**

The Redis counter recovery probe (`tryRecover`) used `cb.PostCheck(nil)`
to close the circuit breaker after a successful liveness probe. That
helper only handles the `HalfOpen -> Closed` transition; from `Open` it
just zeroed the failure counter and left the state at `Open`. The
breaker was therefore stuck `Open` after the first recovery, with two
consequences: `cb.IsHealthy()` returned false until process restart,
and the very next transient Redis error tripped the system back to
local-counter fallback (the breaker's "tolerate N failures" semantic
was silently disabled).

The fix adds an explicit `(cb *CircuitBreaker) Recover()` method that
clears probe state, zeroes the failure counter, and transitions the
breaker straight to `Closed`. `tryRecover` calls it instead of
`PostCheck(nil)`. After a Redis outage the breaker now recovers
cleanly: `IsHealthy()` returns true, the failure counter starts fresh,
and a subsequent transient error is tolerated up to the configured
`failure_threshold` before the breaker re-opens.

**Operator action items after upgrade:**

- No configuration change. The fix is purely a state-machine
  correctness improvement on the existing recovery probe.
- If a deployment was carrying a permanently-stuck breaker after a
  prior Redis blip, the upgrade clears the condition on first
  successful health probe (no manual restart needed).

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
- Use the SQL recipes in [cleanup-and-lifecycle.md#cleanup-queue](../cleanup-and-lifecycle/#cleanup-queue) and [disaster-recovery.md#cleanup-queue-recovery](../disaster-recovery/#cleanup-queue-recovery) to inspect and resolve DLQ entries.

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
  scrubber_interval: "6h"            # Background verification interval (0 = disabled)
  scrubber_batch_size: 100           # Objects per scrub cycle
```

A `verify_on_replicate` field was also parsed from this section, but nothing read it and replicas were never hash-checked. It became a real setting in v0.101.x.

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
