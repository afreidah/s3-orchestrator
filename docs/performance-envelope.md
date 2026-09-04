---
description: "A runbook and results template for characterising throughput and latency, so numbers stay comparable across releases and hardware."
title: "Performance envelope"
---

This page is a **runbook and results template** for characterising the
orchestrator's performance envelope on your own hardware. The tooling in
`loadtest/` produces the per-scenario JSON matrices referenced below; the
results tables here are deliberately empty, for you to fill in after running
the suite.

They are empty because published numbers from someone else's hardware would be
worse than none: throughput here is bounded by the backends and the database,
not by the orchestrator, so a figure without the environment that produced it
cannot be compared against anything. Each table block therefore carries an
**"Environment"** line, and filling that in matters as much as the numbers.

## How to run

### Prerequisites

- Working demo or production environment with at least one configured
  backend and the database reachable from the orchestrator
- `make` and `go` in PATH (vegeta loadtest builds via Makefile target)
- `k6` for the multipart and burst scenarios
- Admin token from `ui.admin_token` (or `S3O_ADMIN_TOKEN` env var) so
  the loadtest binary can call `POST /admin/api/cache/flush` between
  cache-cold runs

### Scenario inventory

Five scenarios produce the matrix:

1. **Sustained PUT** at varying object sizes
   ```bash
   make loadtest-put LOADTEST_SIZES=1024,1048576,104857600 \
     LOADTEST_RATE=200 LOADTEST_DURATION=60s \
     LOADTEST_OUTPUT_JSON=put-sweep.json
   ```

2. **Sustained GET, cache-warm baseline** (second run after a warmup)
   ```bash
   make loadtest-get LOADTEST_SEED=1000 \
     LOADTEST_SIZES=1024,1048576 \
     LOADTEST_RATE=500 LOADTEST_DURATION=60s \
     LOADTEST_OUTPUT_JSON=get-warm.json
   ```

3. **Sustained GET, cache-cold** (flush between each size)
   ```bash
   ./loadtest/s3-loadtest \
     -op get -rate 500 -duration 60s \
     -sizes 1024,1048576 \
     -seed 1000 \
     -cache-flush-before -admin-token "$S3O_ADMIN_TOKEN" \
     -output-json get-cold.json
   ```

4. **Mixed PUT/GET/DELETE** across rates (saturation-find ramp)
   ```bash
   ./loadtest/s3-loadtest \
     -op mixed -rate 100 -ramp-to 2000 -ramp-step 200 \
     -ramp-error-threshold 0.05 \
     -duration 30s -seed 500 \
     -output-json mixed-ramp.json
   ```

5. **List performance** at increasing namespace sizes
   ```bash
   make loadtest-listobjects LOADTEST_SEED=10000   LOADTEST_OUTPUT_JSON=list-10k.json
   make loadtest-listobjects LOADTEST_SEED=100000  LOADTEST_OUTPUT_JSON=list-100k.json
   make loadtest-listobjects LOADTEST_SEED=1000000 LOADTEST_OUTPUT_JSON=list-1m.json
   ```

6. **Concurrent multipart**
   ```bash
   make loadtest-multipart LOADTEST_MPU_CONCURRENCY=10
   make loadtest-multipart LOADTEST_MPU_CONCURRENCY=50
   make loadtest-multipart LOADTEST_MPU_CONCURRENCY=100
   ```

### Out-of-band capture

The vegeta binary captures latency, throughput, and error rate via
vegeta's own metrics. The remaining signals come from existing
surfaces (no in-tree capture pipeline; running a profiler shouldn't
require new code):

| Signal | Source | Method |
|---|---|---|
| Postgres pool utilization | `pg_stat_activity` | `psql -c "SELECT count(*) FROM pg_stat_activity WHERE application_name='s3-orchestrator'"` sampled before and after each run |
| Orchestrator goroutine + heap | `/debug/pprof/goroutine`, `/debug/pprof/heap` | `curl http://orch:9000/debug/pprof/heap > heap.pprof` at saturation |
| Container CPU + RSS | `docker stats` or cgroup `/sys/fs/cgroup/...` | One sample per scenario step |
| Cache hit / miss / size | `/metrics` | Prometheus scrape diff between t=0 and t=end |

### Hardware fingerprint

Record the host's specs in the **Environment** row of each results
table. The loadtest binary already embeds `runtime.GOOS`,
`runtime.GOARCH`, `runtime.NumCPU()`, and the Go version into the
JSON output's `hardware` block; copy that block plus the actual
machine model into each section so the numbers stay interpretable.

## Results

### Scenario 1 - Sustained PUT

**Environment:** _fill from `put-sweep.json -> hardware` + host model_

| Size | RPS achieved | MB/s | P50 ms | P95 ms | P99 ms | Err % |
|---:|---:|---:|---:|---:|---:|---:|
| 1 KB | _TBD_ | _TBD_ | _TBD_ | _TBD_ | _TBD_ | _TBD_ |
| 1 MB | _TBD_ | _TBD_ | _TBD_ | _TBD_ | _TBD_ | _TBD_ |
| 100 MB | _TBD_ | _TBD_ | _TBD_ | _TBD_ | _TBD_ | _TBD_ |

### Scenario 2 - Sustained GET (warm vs cold)

**Environment:** _fill_

| Size | Cold P95 ms | Warm P95 ms | Cold MB/s | Warm MB/s | Cache value (warm/cold latency) |
|---:|---:|---:|---:|---:|---:|
| 1 KB | _TBD_ | _TBD_ | _TBD_ | _TBD_ | _TBD_ |
| 1 MB | _TBD_ | _TBD_ | _TBD_ | _TBD_ | _TBD_ |

### Scenario 3 - Mixed saturation ramp

**Environment:** _fill_

| Requested RPS | Achieved RPS | P95 ms | Err % |
|---:|---:|---:|---:|
| 100 | _TBD_ | _TBD_ | _TBD_ |
| ... | _TBD_ | _TBD_ | _TBD_ |

**Saturation point:** _fill from `mixed-ramp.json -> saturation_rps`_

### Scenario 4 - List performance

**Environment:** _fill_

| Namespace size | P50 ms | P95 ms | P99 ms |
|---:|---:|---:|---:|
| 10 K | _TBD_ | _TBD_ | _TBD_ |
| 100 K | _TBD_ | _TBD_ | _TBD_ |
| 1 M | _TBD_ | _TBD_ | _TBD_ |

Delimiter listings collapse keys into CommonPrefixes in the database via a
loose index scan (recursive CTE skip-scan), so latency tracks the number of
prefixes returned rather than the namespace size; it should stay roughly flat
across these rows.

Plain prefix listings are served index-only from
`idx_object_locations_key_collate_c_covering`, which carries
`(object_key COLLATE "C", created_at)` as its key and the projected
`backend_name`, `size_bytes` and `etag` as payload. Both halves matter, because
`DISTINCT ON` walks every replica row per key and emits one: the payload is what
keeps those rows off the heap, and `created_at` in the key is what lets the
first row of each group win without a sort. On 360 K rows (120 K keys x 3
copies), one 1000-key page reads 51 buffers instead of 3017.

The remaining caveat is the visibility map. An index-only scan still fetches
from the heap for rows on pages not marked all-visible, and `object_locations`
is churned constantly by the replicator and the cleanup queue. Expect a listing
on a busy fleet to land somewhere between the two figures above rather than at
the floor, and autovacuum settings to matter more than they would on a
read-mostly table.

### Scenario 5 - Concurrent multipart

**Environment:** _fill_

| Concurrency | Completed uploads / min | Create P95 ms | Part P95 ms | Complete P95 ms | Err % |
|---:|---:|---:|---:|---:|---:|
| 10 | _TBD_ | _TBD_ | _TBD_ | _TBD_ | _TBD_ |
| 50 | _TBD_ | _TBD_ | _TBD_ | _TBD_ | _TBD_ |
| 100 | _TBD_ | _TBD_ | _TBD_ | _TBD_ | _TBD_ |

## Bottlenecks

After populating the tables above, identify the saturation cause per
scenario. Typical candidates:

- **Backend round-trip latency** dominates at small object sizes
- **Network throughput to backends** caps MB/s at large object sizes
- **Postgres connection pool exhaustion** appears as a spike in P95
  with low CPU on the orchestrator
- **Cache thrashing** when cache-warm and cache-cold P95 converge
  (working set exceeds cache size)
- **Admission control** kicks in via `s3o_admission_rejections_total`
  and `s3o_load_shed_total` non-zero
- **Multipart Postgres contention** on the per-uploadId advisory
  lock at high concurrency

### Known bottleneck: `backend_quotas` row contention

PUT throughput is bounded by row-level lock contention on
`backend_quotas`. Every successful write transaction holds an
`UPDATE backend_quotas SET bytes_used = bytes_used + $size WHERE
backend_name = $name` lock for the duration of the commit, so all
concurrent writes to the same backend serialize on a single row.

Diagnosis pattern in `pg_stat_activity`:

```
state | count | wait_event_type | wait_event
------+-------+-----------------+----------------
active |   200 | Lock            | transactionid
```

with the blocked query being `IncrementQuota` (on the steady-state
write path) or `DecrementQuota` (when displaced-copy cleanup is
running for overwrites). Symptom on the client side: P50 stays sub-ms
but P95/P99 blow out to seconds — most requests are fast, but a tail
queues behind the row lock and admission control sheds them.

Observed wall on the local Nomad demo (3-backend spread, 1 KB objects,
`max_concurrent_writes: 1000`, `max_conns: 200`): ~500 PUT/s before
load shedding fires. Pool size and Postgres `max_connections` do not
move the wall — only the count of contended rows does.

Mitigations available today:

- **Add backends** to spread writes across more `backend_quotas` rows
  (linear scaling of the per-row write rate)
- **Cap concurrent writes per backend** below the lock-serialization
  rate to avoid the wait queue building up

Architectural fix (not in this branch): batch quota deltas in memory
and flush periodically, mirroring the `usage_flush` worker pattern,
so the hot row is updated O(1) times per flush interval instead of
once per write.

## Recommended configuration per scale tier

After running the suite, fill in:

| Tier | Backends | DB pool | Cache max_size | Max concurrent requests | Notes |
|---|---|---:|---:|---:|---|
| Small (< 100 obj/s) | _TBD_ | _TBD_ | _TBD_ | _TBD_ | _TBD_ |
| Medium (100-1k obj/s) | _TBD_ | _TBD_ | _TBD_ | _TBD_ | _TBD_ |
| Large (> 1k obj/s) | _TBD_ | _TBD_ | _TBD_ | _TBD_ | _TBD_ |
