---
title: "Understanding Replication"
description: "How replication works internally: where replicas are placed, what side effects to expect, and how to monitor and tune the workers."
weight: 3
---


This guide explains how the S3 Orchestrator replication system works under the hood — how replicas are created, where they are placed, what side-effects to expect, and how to monitor and tune the process.

For scenario-based walkthroughs, see [Local to Cloud Replication](../minio-cloud-replication/) or [Simple Multi-Cloud Redundancy](../multi-cloud-redundancy/).

## Overview

Replication keeps multiple copies of each object across different backends. When a backend becomes unavailable, reads automatically fail over to a replica — no client-side changes required.

The tradeoff is straightforward: higher replication factors increase durability and read availability at the cost of additional storage consumption, API requests, and egress bandwidth on each provider.

### Two ways a copy gets made

There are two, and which one you use changes the cost accounting throughout this guide.

**The replicator makes it**, which is the default and what most of this page describes. A write lands on one backend, and a background worker later notices the object is short of its factor, reads it back off the backend that holds it, and writes it elsewhere. Every copy therefore costs a GET plus the source backend's egress on top of the PUT.

**The write makes it**, if you set `write_path.parallel_copies.enabled: true`. The write claims several backends up front, uploads to all of them at once from the body it already has in hand, and answers the client as soon as the first copy commits. The remaining copies finish behind the response. No read, no egress on a source backend, and the replicator is left with repair rather than routine copy-making.

The second is off by default because the trade is shape rather than total: the work drops, but those bytes go out at write time instead of spread across replicator cycles, so a deployment whose uplink is the bottleneck can see PUT throughput fall even as its provider bill does. The [Side-Effects and Costs](#side-effects-and-costs) section below quantifies what you stop paying.

## Configuration Reference

Replication is controlled by the `replication` block in your configuration file. A replication factor of 1 (the default) means replication is disabled — each object exists on exactly one backend.

```yaml
replication:
  factor: 2
  worker_interval: 5m
  batch_size: 50
  concurrency: 5
  unhealthy_threshold: 10m
```

| Field | Default | Description |
|---|---|---|
| `factor` | `1` (disabled) | Target number of copies per object. Maximum is the total number of configured backends. |
| `worker_interval` | `5m` | How often the replication worker scans for under-replicated objects. |
| `batch_size` | `50` | Maximum number of under-replicated objects processed per scan. |
| `concurrency` | `5` | Number of objects replicated in parallel within a single scan. |
| `unhealthy_threshold` | `10m` | How long a backend's circuit breaker must be open before the replicator treats it as truly unavailable and creates replacement copies elsewhere. |

All defaults except `factor` are applied automatically when `factor` is set above 1. You only need to specify the fields you want to override.

To have writes place their own copies, add the `write_path` block:

```yaml
write_path:
  parallel_copies:
    enabled: true
    count: 2
    max_in_flight: 256
```

| Field | Default | Description |
|---|---|---|
| `enabled` | `false` | Whether a write places its own copies instead of leaving them to the replicator. |
| `count` | `replication.factor` | Copies a write places. May not exceed the factor - the over-replication cleaner would delete anything past it about as fast as writes created it. A factor of 1 leaves the setting inert. |
| `max_in_flight` | `server.max_concurrent_writes` | Ceiling on writes whose copies are still uploading after their client was answered. A write that finds it full places a single copy and leaves the rest to the replicator. |

Single-object PUTs only. Multipart uploads always write one copy and let the replicator make the rest.

## How the Replicator Works

The replicator is a background worker that runs on a fixed interval. It uses a PostgreSQL advisory lock to ensure only one instance runs at a time across all deployed replicas of the orchestrator.

### Worker lifecycle

1. **Startup pass** — on first boot, the worker runs immediately before entering the ticker loop. This lets it catch up on any replication backlog from downtime or deployment.

2. **Periodic scans** — every `worker_interval` (default 5 minutes), the worker:
   - Identifies backends whose circuit breakers have been open longer than `unhealthy_threshold`
   - Queries the database for up to `batch_size` objects with fewer copies than the target factor (excluding copies on unhealthy backends)
   - Groups results by object key and calculates how many additional copies each object needs
   - Submits copy tasks to a worker pool with the configured `concurrency`

3. **Copy operation** — for each under-replicated object, the worker:
   - Selects a target backend (see [Target Backend Selection](#target-backend-selection))
   - Stream-copies the object from the source backend to the target
   - Records the new replica in the database, cloning all metadata including encryption fields

Each scan emits an audit log entry with the number of copies created, objects checked, and duration.

## Target Backend Selection

When the replicator needs to place a new copy, it iterates through backends **in the order they appear in your configuration file** and picks the first eligible one. This is a first-fit algorithm, not a balanced distribution.

A backend is eligible if it meets all of these conditions:

- It is **not draining**
- It **does not already hold** a copy of the object
- Its **circuit breaker is not open** (the backend is healthy)
- It has **enough free space**: `quota - used_bytes - orphan_bytes >= object_size`

If no backend qualifies, the object remains under-replicated until the next scan.

{{% notice warning %}}
Because selection is first-fit by config order, replicas tend to concentrate on whichever backend appears earliest in your configuration and has available space. With a `pack` routing strategy this effect compounds — primary writes and replicas both favor the same backends, which can fill them unevenly. See [Side-Effects and Costs](#side-effects-and-costs) for details and mitigations.
{{% /notice %}}

## Side-Effects and Costs

Replication is not free. Each replica copy incurs real resource usage on your backend providers:

### API requests

Every replication copy requires **one GET** on the source backend and **one PUT** on the target. If you have 10,000 objects and a replication factor of 2, the replicator will make 10,000 GETs and 10,000 PUTs to create the second copy. These count against each provider's API request limits.

With `write_path.parallel_copies` on, the GET disappears: the write already has the bytes, so the same 10,000 objects cost 10,000 extra PUTs and no reads at all. The PUTs are unavoidable either way - a copy has to be written - so the read is the entire saving, and it is the half that carries egress.

### Bandwidth

The source backend incurs **egress** for each object read, and the target incurs **ingress** for each object written. For providers with egress-based billing (AWS, GCP), this can add up quickly with large datasets.

This is where write-placed copies pay for themselves: no object is read to make a copy, so no source backend is charged egress for one. What remains is the ingress on each target, which both approaches pay identically. The tradeoff is that the ingress arrives during the write rather than during a later replicator cycle, so peak write-time bandwidth rises even though the total falls.

### Storage

Replica bytes count toward the destination backend's quota. A 10 GB dataset with a replication factor of 3 requires 30 GB of total storage across your backends. Size your quotas accordingly.

### Uneven distribution

The first-fit target selection means backends listed earlier in your configuration receive replicas preferentially. In the worst case, one backend can fill to capacity while others remain nearly empty. This is the exact scenario that can occur when using `pack` routing with replication — primary writes fill the first backend, and replicas also target it for other objects.

### Orphan bytes from failures

If a replication copy succeeds on the backend but the database record fails to commit, the object becomes an orphan — it exists on the backend but is not tracked. The cleanup queue will eventually remove it, but until then the orphan bytes reduce available space on that backend.

## Mitigations

Several features work together to keep replication healthy:

- **Place copies during the write** (`write_path.parallel_copies`) to remove the read half of every copy, which is the mitigation with the largest effect on both API counts and egress. It also sidesteps the uneven distribution below, since a write's copies are claimed together from the ranked candidates rather than by a separate first-fit pass.

- **Use `spread` routing** to distribute primary writes across backends evenly, preventing the first-fit replica selection from compounding on already-full backends.

- **The rebalancer** redistributes objects across backends to even out utilization over time, counteracting the uneven distribution that first-fit selection creates.

- **Tune `worker_interval` and `batch_size`** to control replication throughput. A longer interval with a smaller batch size reduces the burst of API requests per scan. A shorter interval with a larger batch catches up faster after outages.

- **`unhealthy_threshold`** prevents premature replacement copies. If a backend has a brief network hiccup, the replicator waits for the threshold to pass before assuming the backend is truly down and creating copies elsewhere. This avoids unnecessary over-replication when the backend recovers moments later.

- **Circuit breakers** prevent the replicator from wasting API requests attempting to copy to a backend that is currently failing. Broken backends are excluded from target selection entirely.

- **Monitor metrics** to catch imbalances early. See [Monitoring](#monitoring) for the full list.

## Over-Replication

Over-replication occurs when an object has more copies than the target factor. The most common cause is backend recovery after a failover: while a backend was down, the replicator created replacement copies elsewhere, and when the original comes back, the object now exists on too many backends.

### Cleanup process

The over-replication cleaner runs on the same interval as the replicator, using its own advisory lock. It queries for objects exceeding the target factor and removes the least valuable copies.

### Scoring

Each copy receives a retention score. Lower scores are removed first:

| Backend State | Score | Rationale |
|---|---|---|
| Draining | 0 | Data is being migrated off — remove first |
| Circuit-broken | 1 | Backend is unreliable — prefer removing from here |
| Healthy | 2.0 – 3.0 | `2 + (1 - utilization_ratio)` — fuller backends score lower, keeping copies on backends with more headroom |

For example, a healthy backend at 90% utilization scores 2.1, while one at 50% scores 2.5. The copy on the 90% backend is removed first, freeing space where it is most needed.

When no quota data is available for a backend, it receives a score of 2.5 (mid-range).

## Encrypted Objects

Encrypted objects are replicated as opaque binary streams. The replicator does not decrypt, re-encrypt, or re-wrap any data encryption keys. The exact same ciphertext that exists on the source backend is written to the target.

All encryption metadata — the wrapped DEK, key ID, and plaintext size — is cloned from the source copy's database record to the new replica's record. This means every replica is byte-identical on the backend and shares the same encryption envelope in the database.

No master key access is required during replication. The replicator only needs read access to the source backend and write access to the target.

## Drain Interaction

When a backend enters the draining state:

- The replicator **excludes it from target selection** — no new replicas are placed on a draining backend
- The over-replication cleaner **prioritizes removing copies from draining backends** (score 0), so excess copies are cleaned from the draining backend first
- The drain manager handles its own object migration independently — if an object already has a replica on another backend, the drain manager simply deletes the draining copy without any data transfer

Draining and replication do not compete. The drain manager moves or deletes objects from the draining backend, while the replicator ensures the target factor is maintained on the remaining healthy backends.

## Rebalancer Interaction

The rebalancer moves objects between backends to even out utilization. These moves do not directly trigger replication — the rebalancer operates on object placement, not replica count.

However, rebalancer moves can create temporary replication imbalances:

- **Temporary under-replication** — if the rebalancer moves an object's primary copy while a replica exists elsewhere, there is a brief window where the database is updating. The next replication scan detects and corrects any shortfall.
- **Temporary over-replication** — a rebalancer move can briefly result in an object existing on more backends than intended. The over-replication cleaner detects and removes excess copies on its next scan.

Both the rebalancer and replicator use separate advisory locks (1001 and 1002), so they can run concurrently. Database-level safeguards — conditional inserts for replica records and compare-and-swap for moves — prevent data inconsistencies from concurrent operations.

## Monitoring

### Replication metrics

| Metric | Type | Description |
|---|---|---|
| `s3o_replication_pending` | Gauge | Objects currently below the target replication factor. Alert if this stays elevated. |
| `s3o_replication_copies_created_total` | Counter | Total replica copies created across all scans. |
| `s3o_replication_errors_total` | Counter | Total replication errors (individual object copy failures). Alert on sustained increase. |
| `s3o_replication_duration_seconds` | Histogram | Time taken for each replication scan cycle. |
| `s3o_replication_runs_total` | Counter | Total scan runs, labeled by `status` (`success` or `error`). Alert on repeated errors. |
| `s3o_replication_health_copies_total` | Counter | Copies created specifically to replace backends that exceeded the unhealthy threshold. |

### Write-placed copy metrics

Only meaningful with `write_path.parallel_copies` on. All four sit idle otherwise.

| Metric | Type | Description |
|---|---|---|
| `s3o_replication_write_copies_committed` | Histogram | Copies committed per write, counting the one that answered the client. This is the metric that says whether the feature is working: sitting at `replication.factor` means writes reach it unaided, drifting toward 1 means they are degrading to a single copy and the replicator is picking the difference back up. |
| `s3o_replication_write_copies_total` | Counter | Further copies by outcome. `committed` landed and was recorded; `untrusted` means a newer write took the key while the copy was still uploading, so it was discarded and replication rebuilds it; `failed` means the backend refused it. |
| `s3o_detached_uploads_depth` | Gauge | Writes whose copies are still uploading after their client was answered. A healthy fleet sits at a handful; a rising floor is a backend going slow without failing. |
| `s3o_replication_write_fanout_skipped_total` | Counter | Writes that placed a single copy because `max_in_flight` was full. Each one costs the read-back the feature exists to avoid, so a sustained rate means either the ceiling is too low for your write rate or a backend is falling behind. |

### Over-replication metrics

| Metric | Type | Description |
|---|---|---|
| `s3o_over_replication_pending` | Gauge | Objects currently above the target replication factor. |
| `s3o_over_replication_removed_total` | Counter | Total excess copies removed. |
| `s3o_over_replication_errors_total` | Counter | Total cleanup errors. Alert on sustained increase. |
| `s3o_over_replication_runs_total` | Counter | Total cleanup runs, labeled by `status` (`success` or `error`). |
| `s3o_over_replication_duration_seconds` | Histogram | Time taken for each cleanup cycle. |

### Rebalancer metrics

| Metric | Type | Description |
|---|---|---|
| `s3o_rebalance_pending` | Gauge | Objects planned for rebalance in the current cycle. |
| `s3o_rebalance_objects_moved_total` | Counter | Total objects moved, labeled by `strategy`. |
| `s3o_rebalance_runs_total` | Counter | Total rebalancer runs, labeled by `strategy` and `status`. |
| `s3o_rebalance_duration_seconds` | Histogram | Rebalancer execution time. |
| `s3o_rebalance_skipped_total` | Counter | Runs skipped, labeled by `reason` (threshold, empty_plan). |

### What to alert on

- **`s3o_replication_pending > 0` for extended periods** — objects are stuck under-replicated. Check backend health, available quota, and circuit breaker state.
- **`s3o_replication_errors_total` increasing steadily** — the replicator is failing to copy objects. Check logs for backend connectivity issues or permission errors.
- **`s3o_replication_runs_total{status="error"}` increasing** — entire scan cycles are failing. This usually indicates a database connectivity issue or advisory lock contention.
- **`s3o_over_replication_pending > 0` for extended periods** — excess copies are not being cleaned up. Check that the cleanup worker is running and backends are reachable for deletions.
- **Backend utilization skew** — compare `s3o_quota_bytes_used` across backends. Large disparities may indicate the first-fit selection is concentrating replicas unevenly.

With write-placed copies on, two more:

- **`s3o_replication_write_copies_committed` drifting below `replication.factor`** — writes are no longer placing all their copies, and the replicator is absorbing the difference along with the read-back you turned the feature on to avoid. The two counters below say why.
- **`s3o_replication_write_fanout_skipped_total` or `..._copies_total{outcome="untrusted"}` rising** — the first means `max_in_flight` is full, so check `s3o_detached_uploads_depth` for a backend going slow; the second means clients are overwriting keys faster than copies finish, and each one costs a rebuild.
