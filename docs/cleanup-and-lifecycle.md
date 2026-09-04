---
description: "The always-on cleanup queue, pending write intents, and lifecycle rules, with the tunables controlling how aggressively each one runs."
title: "Cleanup Queue, Pending Intents, and Lifecycle"
linkTitle: "Cleanup & Lifecycle"
weight: 26
---

## Cleanup queue


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

**Reconcile does not undo a pending delete.** A key with a row in `cleanup_queue` or `cleanup_dlq` is still on the backend only because the delete could not reach it. Reconcile therefore skips importing such a key rather than adopting it, checked inside the import transaction so a cleanup finishing concurrently cannot slip between the check and the insert. Adopting it would resurrect the object: it would come back live, the replicator would spread it to reach the replication factor, and its `created_at` would restart so any lifecycle rule that had expired it would wait another full window.

The suppression is scoped to the `(key, backend)` pair, so a copy that was removed cleanly on another backend is still importable. Suppressed keys are logged and reported as `suppressed_pending_cleanup` in the reconcile result — a run showing many of them is pointing at a cleanup queue that is not draining, not at a reconcile problem.

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


## PUT-before-COMMIT pending intents


The write path can run in two modes. **Direct mode** (`enabled: false`) writes to the backend and commits the metadata immediately afterward; a crash between the two leaks bytes onto the backend with no DB record. **Pending-intent mode** (`enabled: true`, the default) inserts a row into `pending_objects` *before* the backend PUT and atomically deletes that row when the metadata commits — so a crash between the PUT and the commit leaves a recoverable intent the background reaper can resolve.

```yaml
write_path:
  pending_pattern:
    enabled: true        # default: true; PUT-before-COMMIT crash-recovery pattern
    reaper_tick: 1m      # how often PendingReaper sweeps unresolved intents (default: 1m)
    min_age: 5m          # only intents older than this are eligible (default: 5m) — avoids racing in-flight PUTs
    batch_size: 50       # rows claimed per tick (default: 50)
```

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


## Lifecycle (object expiration)


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
- `tags` — [tags](tagging.md) the object must carry, as key/value pairs.
- `expiration_days` — delete objects older than this many days (required, must be > 0).
- At least one of `prefix` or `tags` is required. A rule with neither would expire the whole namespace, so it is refused at startup.
- Omit the `lifecycle` section or leave `rules` empty to disable lifecycle entirely.
- Rules are evaluated every hour by a background worker with an advisory lock.
- Deletions go through the standard `DeleteObject` path — all copies removed, quotas decremented, failed deletes enqueued to the cleanup queue.
- Hot-reloadable via `SIGHUP`.

### Running a sweep now

The scheduled sweep is hourly, and a restart adds up to half an interval of jitter before the first one, so a rule you have just written can take 90 minutes to prove itself. Until it runs, a rule matching nothing is indistinguishable from a rule that ran and found nothing expired.

Trigger one immediately instead:

```bash
s3-orchestrator admin lifecycle
```

```
POST /admin/api/lifecycle
{"status":"ok","deleted":12,"failed":0}
```

A sweep over a large expired backlog runs for minutes, so the endpoint streams a line per object when the caller sends `Accept: application/x-ndjson`; `s3-orchestrator admin lifecycle` and the TUI both do.

`deleted` and `failed` are separate because a sweep that removed nothing since every delete failed is a different answer from one that found nothing to expire. A deployment with no rules configured reports `{"status":"skipped","reason":"no lifecycle rules are configured"}` rather than a sweep of zero — that distinguishes a rule that does not match from config that never reached the process.

The manual sweep does not take the advisory lock the scheduled one holds, matching every other manual trigger, so it can overlap a scheduled tick. Rule application is idempotent and a repeated delete of the same key is a no-op, so the overlap costs duplicated work rather than correctness.

### How a filter matches

Every condition a rule sets must hold, so a rule carrying both a prefix and tags selects their intersection. Several tags are likewise an "and": the object has to carry all of them, matching on the value and not the key alone.

There is no "or" inside a rule. Express it by writing a second rule — rules are evaluated independently, each against its own cutoff, so an object matching two of them is deleted as soon as the earlier window passes regardless of the order they appear in.

Two rules may share a prefix as long as their tags differ, since they then select different objects. Two rules with the same filter are refused as a duplicate.

### Age is measured from object creation

The cutoff compares against the object's creation time, not the time a tag was applied — the same as S3. Tagging an object that is already older than a rule's window therefore makes it eligible on the next sweep, so a rule expiring `retain=30d` after 30 days will delete a two-year-old object shortly after you tag it.

That is the intended behaviour, but it makes tagging an existing object a deletion decision rather than a scheduling one. Worth knowing before retagging in bulk.

