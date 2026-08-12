---
title: "Background Services"
linkTitle: "Background Services"
weight: 30
---

# Background Services

The orchestrator runs a set of long-running background workers that keep the metadata layer consistent with the backends, handle replication / cleanup / lifecycle, and refresh observability state. All locked tasks apply a random startup jitter of up to half the tick interval before the first tick, preventing thundering herd on the advisory lock when multiple instances start simultaneously.

## Worker reference

| Task | Interval | Advisory Lock | Description |
|------|----------|:---:|-------------|
| **Usage flush + metrics** | configurable (default 30s) | When Redis configured | Flushes usage counters to PostgreSQL, then refreshes quota stats, usage baselines, object counts, and multipart counts. Updates Prometheus gauges. Adaptive mode shortens interval near limits. Advisory lock is acquired whenever Redis is configured (regardless of health) to prevent double-counting during recovery. |
| **Stale multipart cleanup** | 1h | Yes | Aborts multipart uploads older than 24h and deletes their temporary part objects. |
| **Cleanup queue** | 1m | Yes | Retries failed backend object deletions with exponential backoff (1m to 24h, max 10 attempts). On the tenth consecutive failure the row graduates to `cleanup_dlq` for operator action; `orphan_bytes` stays incremented because the bytes are still on disk. |
| **Rebalancer** | configurable (default 6h) | Yes | Moves objects between backends per strategy. Only runs when enabled. |
| **Replicator** | configurable (default 5m) | Yes | Creates copies of under-replicated objects. Only runs when factor > 1. Runs once at startup. |
| **Over-replication cleaner** | configurable (default 5m) | Yes | Removes excess copies of objects that exceed the replication factor. Only runs when factor > 1. |
| **Lifecycle** | 1h | Yes | Deletes objects matching lifecycle rules whose `created_at` exceeds `expiration_days`. Only runs when rules are configured. |
| **Reconciler** | configurable (default 24h) | Yes | Diffs each backend's key listing against the object ledger, importing objects the ledger is missing and removing rows for objects the backend no longer holds. One pass covers every virtual bucket sharing the backend. Only runs when `reconcile.enabled: true`. |
| **Pending reaper** | configurable (default 1m) | Yes | Resolves PUT-before-COMMIT intents that survived a failed metadata commit. HEADs the destination backend and either promotes the row into `object_locations` (object present) or drops the intent (object absent). Skips intents younger than `min_age` so in-flight PUTs are not interrupted. |
| **Scrubber** | configurable (default 6h) | Yes | Works through the copies least recently verified, re-hashes them, and discards any whose bytes no longer match the stored `content_hash`. Only runs when `integrity.enabled: true` and `scrubber_interval > 0`, and only on a tick - never at startup, so an interval longer than the process lifetime means it never runs. Backends that have spent their usage allowance are excluded from the batch and their copies reported as deferred, which leaves the coverage age climbing rather than reporting an unverified fleet as checked. |
| **Notification drainer** | 5s | No | Drains `notification_outbox` rows by POSTing CloudEvents JSON to configured webhook endpoints. Optional HMAC signing per endpoint. |
| **CB watchdog** | 1m | No | Checks all circuit breakers for stale half-open probes. If a probe has been in flight longer than 2 minutes, resets the circuit to open so a new probe can be dispatched. Prevents circuits from getting stuck half-open when traffic stops. |

## Concurrency

Background services (rebalancer, replicator, over-replication cleaner, cleanup queue) share the admission semaphore with HTTP requests, so `max_concurrent_requests` is the total budget for both HTTP and background backend operations.

## Multi-instance behavior

The advisory locks are PostgreSQL session-scoped - if one instance holds the lock for a task, other instances skip that tick silently. Each task runs on exactly one instance at any moment. The Notification drainer and CB watchdog are not locked because they're idempotent and per-instance state is acceptable for them.

See [docs/deployment.md](deployment.md) for the multi-instance deployment model.
