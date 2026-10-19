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

`multipart_stale_timeout` is consumed by the hourly `CleanupStaleMultipartUploads` sweep - uploads that have been open longer than this are aborted, their parts deleted from the backend, and the multipart rows removed. The default 24h matches the AWS S3 SDK's default abort behavior; lower it on backends with tight free-tier headroom to recover quota faster.

Every transaction that stops recording bytes queues their deletion in the same transaction: client deletes (DeleteObject and DeleteObjects), overwrites by PUT, CopyObject and multipart completion, drain, rebalance and rewrite moves removing their source, scrubber and verify-on-read integrity failures, over-replication removal, multipart abort and completion removing parts, a discarded intent, and a refused placement. The rows are inserted already claimed by the instance running the transaction. After the commit that instance deletes the bytes right away, one batched request per backend, and settles each row on its own result: retired when the delete succeeds or returns 404, retried with backoff when it fails. The request path and the worker settle rows through the same code. Outcomes are written even when the client disconnects mid-delete, so a hang-up leaves the row retried rather than stuck.

Each row names the path to delete in `storage_key`, the path of the one write's bytes it is for, so a delete can never reach another write's copy of the same key. `object_key` rides along so an operator can tell which object an orphan belonged to.

Each row carries the bytes' `size_bytes`. A backend's orphan bytes are the sum of `size_bytes` over its `cleanup_queue` and `cleanup_dlq` rows, computed by the `backend_capacity` view, so write admission and replica target selection account for space that is not yet released. Nothing maintains a counter: inserting a row adds its bytes and removing it releases them.

**Per-row claim pattern.** Every row carries `claimed_at` and `claimed_by` columns. A row queued by a transaction is claimed by that instance from the start. When a worker tick fetches a batch it stamps each row with the current instance's identifier and timestamp, gated by `FOR UPDATE SKIP LOCKED` (Postgres) or SQLite's single-writer serialisation, so two instances ticking concurrently always see disjoint row sets. A claim older than `claim_grace_period` (default 5m) is reclaimable, so a process that died before settling its rows does not leave them stuck; reclaims emit `s3o_cleanup_queue_stale_claims_recovered_total` and a `cleanup_queue.claim_recovered` audit event.

The background worker runs every minute and retries with exponential backoff (1 minute to 24 hours). Each tick claims due rows 50 at a time, the earliest `next_retry` first, and keeps claiming for up to 20 batches; it stops early on a batch shorter than 50, since the queue has run dry, or on one that settles nothing, since the backends are refusing. Each batch is deleted with one batched request per backend, then every row is settled on its own result; the rows whose bytes are gone are retired together in one statement. Every settle write matches the claim the row was read under, so a row another instance reclaimed after the grace period is left to its new holder. Scheduling a retry clears the row's claim so it is immediately re-eligible for the next tick. After 10 failed attempts, the row is graduated to the `cleanup_dlq` table by `MoveCleanupToDLQ`, one statement that deletes the queue row and inserts it into `cleanup_dlq`. The bytes are still on disk, so they stay counted as orphan bytes while the DLQ row exists. The DLQ entry retains the full row payload (key, storage key, backend, size, reason, last_error) plus an `original_id` correlation column so an operator can find the original queue entry.

**Reconcile does not undo a pending delete.** A key with a row in `cleanup_queue` or `cleanup_dlq` is on the backend only because its delete has not completed. The row exists before the bytes are deleted, so there is no window in which bytes awaiting deletion have no row. Reconcile therefore skips importing such a key rather than adopting it. Each listing page is classified with one query, and the check is repeated inside the import transaction so a cleanup finishing concurrently cannot slip between the check and the insert. Adopting it would resurrect the object: it would come back live, the replicator would spread it to reach the replication factor, and its `created_at` would restart so any lifecycle rule that had expired it would wait another full window.

The suppression is scoped to the `(storage_key, backend)` pair, so a copy that was removed cleanly on another backend is still importable. Suppressed keys are logged and reported as `suppressed_pending_cleanup` in the reconcile result - a run showing many of them is pointing at a cleanup queue that is not draining, not at a reconcile problem.

**Monitoring:**

- `s3o_cleanup_queue_depth` staying elevated - orphaned objects are accumulating in the active queue. The gauge briefly includes rows the request path is settling.
- `s3o_cleanup_queue_enqueued_total{reason}` - every row queued for deletion, by reason. It tracks the delete and overwrite rate.
- `s3o_cleanup_queue_processed_total{status}` - rows settled, on the request path and by the worker.
- `s3o_cleanup_queue_processed_total{status="exhausted"}` - counter increments each time an item exhausts retries.
- `s3o_cleanup_queue_processed_total{status="success_absent"}` - counter increments each time a backend DELETE returned 404 and the row was dropped as idempotent success (the backend already agrees the object is gone). A sustained rate here is benign and just means upstream PUTs are silently failing somewhere; spikes are worth correlating with backend health.
- `s3o_cleanup_queue_processed_total{status="claim_lost"}` - rows another instance took over before this one settled them. Every settle write matches the claim the row was read under, so the new holder settles the row and this one leaves it alone. A sustained rate means settles take longer than `claim_grace_period`.
- `s3o_cleanup_queue_stale_claims_recovered_total{backend}` - non-zero rate means a process died before settling its rows or the grace period is too short for realistic worst-case processing time.
- `s3o_cleanup_dlq_depth > 0` - the DLQ holds at least one unrecoverable orphan; alerting here gives operators a direct signal instead of a counter delta.
- `s3o_cleanup_dlq_enqueued_total{backend}` - rate of graduations per backend; a single backend dominating means that backend's delete path is broken.
- `s3o_quota_orphan_bytes` - elevated values mean backends have significant physically unreleased space (DLQ entries are the long-tail contributors).

**Manual cleanup:** Inspect DLQ entries and resolve them deliberately. The bytes are still on the backend, so the workflow is *delete the object out-of-band, then write off the row*. Removing the row releases its bytes from the backend's orphan bytes:

```sql
-- View unrecoverable orphans needing manual intervention
SELECT id, original_id, backend_name, object_key, storage_key, reason, attempts,
       size_bytes, first_enqueued_at, moved_at, last_error
FROM cleanup_dlq
ORDER BY moved_at;

-- After confirming the object is gone (manual S3 delete, reconciler sweep, etc.):
DELETE FROM cleanup_dlq WHERE id = 42;

-- Or, to push a DLQ entry back through automatic retry (e.g. after fixing the backend):
INSERT INTO cleanup_queue (backend_name, object_key, storage_key, reason, size_bytes, next_retry, attempts, last_error)
SELECT backend_name, object_key, storage_key, reason, size_bytes, NOW(), 0, last_error
  FROM cleanup_dlq WHERE id = 42;
DELETE FROM cleanup_dlq WHERE id = 42;
```


## PUT-before-COMMIT pending intents


Every write claims its target by inserting a row into `pending_objects` *before* its bytes land, and the transaction that records the copy deletes that row. That covers client PUTs and their extra copies, replicas, drain and rebalance moves, the rewrite passes (encrypt, decrypt, compress and decompress existing) and multipart parts. The insert admits the bytes against the backend's room, so a write cannot overfill a backend, and a crash between the upload and the commit leaves a recoverable intent the background reaper can resolve. Intents cannot be turned off.

```yaml
write_path:
  pending_pattern:       # the pattern is always on; only the reaper is tunable
    reaper_tick: 1m      # how often PendingReaper sweeps unresolved intents (default: 1m)
    min_age: 5m          # only intents older than this are eligible (default: 5m) - avoids racing in-flight PUTs
    batch_size: 50       # rows claimed per tick (default: 50)
```

**How recovery works.** On every tick the `PendingReaper` worker (`internal/worker/pending.go`) claims a batch of `pending_objects` rows older than `min_age`, HEADs the backend at the intent's recorded `storage_key`, and resolves each one:

- **HEAD 200** -> the backend received the bytes. Promote the intent to a committed `object_locations` row (`pending_reaper.promoted` audit event).
- **HEAD 404** -> the backend never received the bytes. Drop the intent (`pending_reaper.dropped` audit event). No orphan exists.
- **Non-404 HEAD error** -> leave the intent for the next tick. A sustained backend reachability problem here surfaces as `s3o_pending_intents_resolved_total{status="ambiguous"}`.
- **A later write for the same key already committed** -> drop the intent as superseded and queue its bytes for deletion under reason `superseded_intent` (`pending_reaper.superseded`).
- **A companion intent** (an extra PUT copy, a replica, a move, a rewrite or a multipart part) is never promoted. It is kept when a recorded copy sits at its path (`pending_reaper.companion_kept`); otherwise its bytes are queued for deletion under reason `companion_discarded` (`pending_reaper.companion_discarded`). A copy that died mid-transfer is cleaned up this way.

**Why `min_age` matters.** The reaper must not race the foreground write path; if `min_age` is too short the reaper can interrogate an intent whose backend PUT is still in flight and either prematurely commit it or churn `ambiguous` resolutions. The 5-minute default is generous; lower it only if you have measured the p99 PUT duration and accept the operational tradeoff.

**Monitoring:**

- `s3o_pending_intents_enqueued_total` - intents inserted, by every write that claims a target: PUTs, extra copies, replicas, moves, rewrites and parts.
- `s3o_pending_intents_resolved_total{status}` - `committed` is the happy path (synchronous commit succeeded); `promoted`, `dropped`, `superseded`, `companion_kept` and `companion_discarded` are reaper resolutions; `ambiguous` is the alert.
- `s3o_pending_intents_depth` - gauge of unresolved intents. Alert when consistently above `batch_size` - the reaper is not keeping up (raise `batch_size`, lower `reaper_tick`, or add concurrency).
- Audit events: `pending_reaper.promoted` / `pending_reaper.dropped` / `pending_reaper.superseded` / `pending_reaper.companion_kept` / `pending_reaper.companion_discarded`.


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

- `prefix` - key prefix to match.
- `tags` - [tags](tagging.md) the object must carry, as key/value pairs.
- `expiration_days` - delete objects older than this many days (required, must be > 0).
- At least one of `prefix` or `tags` is required. A rule with neither would expire the whole namespace, so it is refused at startup.
- Omit the `lifecycle` section or leave `rules` empty to disable lifecycle entirely.
- Rules are evaluated every hour by a background worker with an advisory lock.
- Deletions go through the standard `DeleteObject` path - all copies removed, quotas decremented, and the bytes queued in the cleanup queue and deleted after the commit.
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

`deleted` and `failed` are separate because a sweep that removed nothing since every delete failed is a different answer from one that found nothing to expire. A deployment with no rules configured reports `{"status":"skipped","reason":"no lifecycle rules are configured"}` rather than a sweep of zero - that distinguishes a rule that does not match from config that never reached the process.

The manual sweep does not take the advisory lock the scheduled one holds, matching every other manual trigger, so it can overlap a scheduled tick. Rule application is idempotent and a repeated delete of the same key is a no-op, so the overlap costs duplicated work rather than correctness.

### How a filter matches

Every condition a rule sets must hold, so a rule carrying both a prefix and tags selects their intersection. Several tags are likewise an "and": the object has to carry all of them, matching on the value and not the key alone.

There is no "or" inside a rule. Express it by writing a second rule - rules are evaluated independently, each against its own cutoff, so an object matching two of them is deleted as soon as the earlier window passes regardless of the order they appear in.

Two rules may share a prefix as long as their tags differ, since they then select different objects. Two rules with the same filter are refused as a duplicate.

### Age is measured from object creation

The cutoff compares against the object's creation time, not the time a tag was applied - the same as S3. Tagging an object that is already older than a rule's window therefore makes it eligible on the next sweep, so a rule expiring `retain=30d` after 30 days will delete a two-year-old object shortly after you tag it.

That is the intended behaviour, but it makes tagging an existing object a deletion decision rather than a scheduling one. Check the rule's window before retagging in bulk.

