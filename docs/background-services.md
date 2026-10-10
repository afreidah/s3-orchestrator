---
description: "The long-running workers keeping metadata consistent with the backends: replication, cleanup, lifecycle, and observability refresh."
title: "Background Services"
linkTitle: "Background Services"
weight: 32
---

The orchestrator runs a set of long-running background workers that keep the metadata layer consistent with the backends, handle replication / cleanup / lifecycle, and refresh observability state. All locked tasks apply a random startup jitter of up to half the tick interval before the first tick, preventing thundering herd on the advisory lock when multiple instances start simultaneously.

## Worker reference

| Task | Interval | Advisory Lock | Description |
|------|----------|:---:|-------------|
| **Usage flush** | configurable (default 30s) | When Redis configured | Flushes usage counters to the metadata store, refreshes this instance's usage baselines, and reloads the cached drain states. With Redis, every instance also loads the published fleet snapshot and worker gauges. Runs on every instance in every mode. Adaptive mode shortens interval near limits. Advisory lock is acquired whenever Redis is configured (regardless of health) to prevent double-counting during recovery. |
| **Fleet snapshot** | `telemetry.metrics.fleet_interval` (default 60s) | Yes | Computes the fleet-wide figures - quota bytes, per-backend ledger counts (objects, unverified, plaintext, unreadable, compression, verification coverage), multipart counts, and the under- and over-replicated counts - publishes them to the Prometheus gauges, and with Redis shares them with the other instances. The dashboard and `/admin/api/status` read the same snapshot, so their figures can be up to one interval old. An instance that finds a snapshot younger than half the interval applies it instead of recomputing, so the ledger is scanned once per interval however many instances run. Runs on every instance in every mode. A rebalance, replication, over-replication, reconcile or reload pass that changes something refreshes it immediately. |
| **Stale multipart cleanup** | 1h | Yes | Aborts multipart uploads older than 24h and deletes their temporary part objects. |
| **Cleanup queue** | 1m | Yes | Retries failed backend object deletions with exponential backoff (1m to 24h, max 10 attempts). Each tick claims due rows 50 at a time, oldest retry first, for up to 20 batches; it stops early on a batch shorter than 50 or one that settles nothing. On the tenth consecutive failure the row graduates to `cleanup_dlq` for operator action; `orphan_bytes` stays incremented because the bytes are still on disk. |
| **Rebalancer** | configurable (default 6h) | Yes | Moves objects between backends per strategy. Only runs when enabled. |
| **Replicator** | configurable (default 5m) | Yes | Creates copies of under-replicated objects. Only runs when factor > 1. Runs once at startup. |
| **Over-replication cleaner** | configurable (default 5m) | Yes | Removes excess copies of objects that exceed the replication factor. Only runs when factor > 1. |
| **Backend drain** | 10s | Yes | Works every backend whose drain record is in progress: aborts its open multipart uploads, then moves its objects off 100 at a time, smallest first. An object another backend also holds loses only its draining copy, and the rest move to the least-utilized eligible backend. A draining copy that holds the only usable encryption key is kept and the object counts as failed, so the drain cannot complete while it remains. An object that fails to move is passed over for the rest of the pass and retried on the next tick. Once the pass has walked every object, the drain is marked `drained` if no object rows, in-flight intents, or multipart uploads remain, and `failed` with the reason on an error it cannot retry. Does nothing when no drain is in progress. See [draining a backend](operations.md#draining-a-backend). |
| **Lifecycle** | 1h | Yes | Deletes objects matching lifecycle rules whose `created_at` exceeds `expiration_days`. Only runs when rules are configured. Trigger one on demand with `POST /admin/api/lifecycle` or `s3-orchestrator admin lifecycle`. |
| **Reconciler** | configurable (default 24h) | Yes | Diffs each backend's key listing against the object ledger one listing page at a time, classifying every key on the page with a single query, importing objects the ledger is missing and removing rows for objects the backend no longer holds. A key whose delete is still outstanding - waiting in `cleanup_queue` or dead-lettered to `cleanup_dlq` - is left alone rather than imported, because those bytes are an orphan awaiting cleanup and importing them would undo the delete. Such keys are logged and counted as `suppressed_pending_cleanup`. An imported object is classified by its bytes: a seek table marks it as compressed and supplies the logical size, since nothing else can once every row for it is gone. One pass covers every virtual bucket sharing the backend. A backend with a drain record is not scanned, so nothing found on it is imported back while it drains or after it has drained. Only runs when `reconcile.enabled: true`. |
| **Pending reaper** | configurable (default 1m) | Yes | Resolves PUT-before-COMMIT intents that survived a failed metadata commit. HEADs the destination backend and either promotes the row into `object_locations` (object present) or drops the intent (object absent). Skips intents younger than `min_age` so in-flight PUTs are not interrupted. |
| **Scrubber** | configurable (default 6h) | Yes | Works through the copies least recently verified, re-hashes them, and discards any whose bytes no longer match the stored `content_hash`. The stored form is undone before hashing, in the reverse of the order it was applied - decrypt, then decompress - because `content_hash` covers the bytes the client wrote. A copy it cannot decode at all is reported as unreadable rather than corrupt and left alone, since a copy that was never read has not been judged. Only runs when `integrity.enabled: true` and `scrubber_interval > 0`, and only on a tick - never at startup, so an interval longer than the process lifetime means it never runs. Backends that have spent their usage allowance are excluded from the batch and their copies reported as deferred, which leaves the coverage age climbing rather than reporting an unverified fleet as checked. |
| **Notification drainer** | 5s | No | Drains `notification_outbox` rows by POSTing CloudEvents JSON to configured webhook endpoints. Optional HMAC signing per endpoint. |
| **CB watchdog** | 5s | No | Probes every registered circuit breaker. A backend breaker that is open runs a health check against its backend once its backoff is due, and closes when the check passes, so recovery does not wait for client traffic. A breaker with a half-open probe that has been in flight too long is reset to open so a new probe can be dispatched. |

## Manual triggers

Every worker above can be run on demand rather than waited for, from the admin API, `adminctl`, the TUI and the dashboard: rebalance, replicate, over-replication, scrub, backfill-checksums, reconcile, usage-flush and lifecycle.

A manual pass deliberately does not take the advisory lock the scheduled tick holds, so it can overlap a scheduled one. Every pass is idempotent - a repeated delete of the same key is a no-op - so the overlap costs a little duplicated work rather than correctness.

Lifecycle is the one where this matters most in practice. Its tick is hourly and a restart adds up to half that again as jitter, so a rule you have just written can take 90 minutes to prove itself. Until it runs, a rule that matches nothing looks exactly like a rule that ran and found nothing expired; the trigger reports deleted and failed counts separately so you can tell those apart immediately.

## Concurrency

Background services (rebalancer, replicator, over-replication cleaner, cleanup queue) share the admission semaphore with HTTP requests, so `max_concurrent_requests` is the total budget for both HTTP and background backend operations.

## Multi-instance behavior

The advisory locks are PostgreSQL session-scoped - if one instance holds the lock for a task, other instances skip that tick silently. Each task runs on exactly one instance at any moment. The Notification drainer and CB watchdog are not locked because they're idempotent and per-instance state is acceptable for them.

See [docs/deployment.md](deployment.md) for the multi-instance deployment model.
