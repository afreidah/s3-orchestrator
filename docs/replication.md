---
title: "Replication, Over-Replication, and Reconciliation"
linkTitle: "Replication"
weight: 25
---

## Replication


Creates additional copies of objects on different backends for redundancy.

```yaml
replication:
  factor: 2                      # copies per object (default: 1 = no replication)
  worker_interval: "5m"          # replication cycle (default: 5m)
  batch_size: 50                 # objects per cycle (default: 50)
  concurrency: 5                 # parallel object replications per cycle (default: 10)
  unhealthy_threshold: "10m"     # grace period before replacing copies on circuit-broken backends (default: 10m)
```

The replication factor must be `<= number of backends`. The worker runs once at startup to catch up on any pending replicas, then continues at the configured interval. Reads automatically fail over to replicas if the primary copy is unavailable.

Replication is **asynchronous** — writes go to a single backend and the replicator creates additional copies in the background. When a client overwrites an existing key, all old copies (including replicas) are removed and a single new copy is written. The replication factor drops to 1 until the next replicator cycle creates the additional copies. If the single backend holding the new copy fails before replication runs, the new version of the object is at risk. For most workloads this window (up to `worker_interval`) is acceptable. Lowering `worker_interval` reduces the exposure at the cost of more frequent DB queries and backend I/O.

**Health-aware replication:** When backend circuit breakers are enabled, the replicator monitors backend health. If a backend's circuit breaker has been open longer than `unhealthy_threshold`, copies on that backend are treated as unavailable and replacement copies are created on healthy backends. This prevents sustained outages from silently reducing redundancy. The threshold prevents churn during brief transient failures. Set to `0` to disable health-aware replication (copies on down backends are still counted).

**Verifying new copies:** Copies are streamed backend-to-backend and recorded without being read back, so a copy that lands corrupt still counts toward the replication factor until the scrubber reaches it. Setting `integrity.verify_on_replicate` closes that window by hash-checking each new copy before recording it, at the cost of reading every replica back. It is off by default; see [configuration.md#integrity](../configuration/#integrity).


## Rebalance


Moves objects between backends to optimize storage distribution. Disabled by default — enabling it will generate egress/ingress traffic on your backends.

```yaml
rebalance:
  enabled: true
  strategy: "pack"               # "pack" or "spread" (default: pack)
  interval: "6h"                 # default: 6h
  batch_size: 100                # objects per run (default: 100)
  threshold: 0.1                 # min utilization spread to trigger (default: 0.1)
  concurrency: 5                 # parallel moves per run (default: 10)
```

- **pack** — fills backends in config order, consolidating free space onto the last backend. Good for maximizing free-tier allocations.
- **spread** — equalizes utilization ratios across all backends. Good for distributing load.

Object moves run concurrently within each batch, bounded by `concurrency`. Increase for faster rebalancing; decrease to reduce backend load.


## Over-replication cleanup



When a backend recovers after the replicator has already created replacement copies on other backends, objects end up with more copies than the replication factor. A background worker detects and removes the excess.

The cleaner scores each copy by its backend's health and storage utilization, then removes the lowest-scoring copies until the object reaches the target factor:

- **Draining backend**: score 0 (always removed first)
- **Circuit-broken backend**: score 1 (removed next)
- **Healthy backend**: score 2 + (1 − utilization ratio), range [2..3]

Among healthy backends, the most utilized backend gets the lowest score — freeing space where it is scarcest. Each object's copies are locked with `FOR UPDATE` to prevent races with concurrent replicator or rebalancer activity.

The worker runs at the `replication.worker_interval` and shares the same `batch_size` and `concurrency` settings. It only runs when `replication.factor > 1`. Like the replicator, it uses a PostgreSQL advisory lock for multi-instance coordination.

Cleanup can also be triggered on demand via the admin API (`POST /admin/api/over-replication`), the CLI (`s3-orchestrator admin over-replication --execute`), or the web dashboard's **Clean Excess** button.

## Orphan reconciliation


Optional background service that periodically scans each backend's S3 bucket and reconciles it against the metadata database. For each backend, it walks both sides as ascending key streams — S3 paginated by `ListObjects` and the DB paginated by `ListObjectsByBackendKeyAsc` — and merges them in lockstep. Keys present only on the backend are imported; keys present only in the DB are removed. Memory is bounded by the page size on each side (1000 entries) regardless of object count, so backends holding millions of objects reconcile without OOM. Objects are imported at their literal backend key, including keys outside every configured virtual bucket prefix; those rows are marked unmanaged, so they count toward the backend's quota without replication, rebalance, integrity or drain acting on them.

```yaml
reconcile:
  enabled: true       # disabled by default
  interval: "24h"     # how often to run (default: 24h)
```

Disabled by default. Requires a restart to enable/disable (non-reloadable). Runs under advisory lock `1009` to prevent concurrent scans across instances.

On-demand reconciliation is available via the admin API — useful after backend data loss or token expiry events:

```bash
# Reconcile all backends
s3-orchestrator admin reconcile

# Reconcile a single backend
curl -X POST -H "X-Admin-Token: $TOKEN" \
  http://localhost:9000/admin/api/reconcile?backend=g3
```
