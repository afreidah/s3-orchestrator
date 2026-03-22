This guide covers configuration knobs that affect throughput, latency, and resource usage.

## Connection Pool Sizing

```yaml
database:
  max_conns: 50          # max pool connections (default: 50)
  min_conns: 5           # min idle connections (default: 5)
  max_conn_lifetime: "5m" # max connection age (default: 5m)
```

### Sizing Formula

Each S3 request uses at least one database connection. Background workers (rebalancer, replicator, cleanup, usage flush) each hold a connection during their tick. Advisory lock acquisition uses a dedicated connection.

```
max_conns = max_concurrent_requests + (active_workers × 2) + 10
```

| Workload | max_concurrent_requests | Recommended max_conns |
|----------|------------------------|----------------------|
| Dev / testing | 10 | 50 (default) |
| Low traffic (<100 RPS) | 50 | 50 (default) |
| Medium traffic (100–500 RPS) | 100 | 100–150 |
| High traffic (500+ RPS) | 200+ | 200+ |

### Guidelines

- **Multi-instance deployment**: divide your PostgreSQL `max_connections` across instances. Each instance's `max_conns` should be `(max_connections - 20) / instance_count` (reserve 20 for superuser and monitoring connections).
- `min_conns` keeps idle connections warm. Set it to roughly half of `max_conns` to avoid connection setup latency on traffic spikes.
- `max_conn_lifetime` rotates connections to pick up DNS changes and distribute load across replicas. The 5-minute default works well in most environments.
- Monitor `pgx_pool_acquired_conns` / `pgx_pool_total_conns` in Prometheus. If acquired consistently equals total, the pool is saturated.

## Admission Control

```yaml
server:
  max_concurrent_requests: 1000  # default: 1000 (applied when split pools are not configured)
  # max_concurrent_reads: 0     # separate limit for GET/HEAD (0 = use global)
  # max_concurrent_writes: 0    # separate limit for PUT/POST/DELETE (0 = use global)
  # load_shed_threshold: 0      # 0.0-1.0, start shedding at this capacity ratio (0 = disabled)
  # admission_wait: "0s"        # brief wait before rejection (0 = instant)
```

Limits the number of S3 requests being processed concurrently. When the limit is reached, new requests are rejected with `503 SlowDown` and a `Retry-After: 1` header instead of queueing and potentially exhausting backend connections and goroutines.

### Global vs split pools

By default, `max_concurrent_requests` applies a single limit to all operations. For workloads where write storms should not starve reads, set `max_concurrent_reads` and `max_concurrent_writes` instead. When both are set, reads (GET, HEAD) and writes (PUT, POST, DELETE) get independent semaphores and cannot block each other.

### Active load shedding

When `load_shed_threshold` is set (e.g. `0.8`), the server begins probabilistically rejecting requests before the hard limit. The rejection probability ramps linearly from 0% at the threshold to 100% at full capacity. This provides a smooth degradation curve instead of a cliff at the admission limit.

### Admission wait

When `admission_wait` is set (e.g. `50ms`), requests briefly wait for a slot to free up before being rejected. This smooths micro-bursts where a slot opens up within milliseconds. The request's context deadline is also respected — whichever fires first wins. Set to `0` (default) for instant rejection.

### When to set this

- **Default (0)**: no limit. Fine for low-traffic single-instance deployments.
- **Recommended starting point**: set to 2-3x your `database.max_conns` value. Every S3 operation needs at least one database query, so there's no benefit admitting more requests than the connection pool can serve.
- **Split pools**: if your workload is read-heavy, set `max_concurrent_reads` higher than `max_concurrent_writes` to protect read availability during upload bursts.
- **Load shedding**: start with `load_shed_threshold: 0.8` and adjust based on `s3o_load_shed_total` rates.
- **Admission wait**: `50ms` to `100ms` is a good starting point for smoothing bursts without adding noticeable latency.

### Monitoring

- `s3o_admission_rejections_total` — requests rejected at the hard admission limit
- `s3o_load_shed_total` — requests probabilistically shed before the hard limit
- `s3o_early_rejections_total` — uploads rejected before body transmission (no backend capacity)
- `s3o_inflight_requests` — gauge of currently processing requests by method. Use this to find the right limit value.

## Backend Timeout

```yaml
server:
  backend_timeout: "30s"
```

This timeout applies to each individual S3 API call to a backend (GET, PUT, HEAD, DELETE). It does not cover the full request lifecycle — just the backend leg.

### Provider-Specific Recommendations

| Provider | Recommended Timeout | Notes |
|----------|-------------------|-------|
| MinIO (local) | `5s` | Local network, should be very fast |
| AWS S3 | `15-30s` | Generally fast, occasional slow first-byte for large objects |
| Backblaze B2 | `30s` | Consistent but not the fastest |
| OCI Object Storage | `30-60s` | Can have slower first-byte times, especially for large objects |
| Cloudflare R2 | `15-30s` | Generally fast |

If you see frequent `context deadline exceeded` errors in logs, increase the timeout. If backends are consistently fast, lowering it helps fail fast when a backend is unhealthy.

## HTTP Server Timeouts

```yaml
server:
  read_header_timeout: "10s"  # protects against slowloris attacks
  read_timeout: "5m"          # total time to read request (including body)
  write_timeout: "5m"         # total time to write response
  idle_timeout: "120s"        # keep-alive idle time
```

### Large Object Workloads

For environments that regularly handle multi-gigabyte objects:

- **Increase `read_timeout` and `write_timeout`** to `15m` or `30m`. A 5 GB upload at 100 MB/s takes ~50 seconds, but slower networks or larger objects need more headroom.
- `read_header_timeout` should stay low (10s) since it only covers headers, not the body.
- `idle_timeout` at 120s is generous for most clients. Lower to 60s if you want to reclaim connections faster.

### High-Connection Environments

If you serve thousands of concurrent clients:

- Lower `idle_timeout` to `30-60s` to reduce connection count.
- Monitor open file descriptors and increase ulimits if needed.

## OS and Container Tuning

### File descriptor limits

The orchestrator holds many concurrent connections: client requests, backend S3 calls, database pool, and background workers (replicator, rebalancer, cleanup). Set the container's nofile ulimit to at least `65535` to avoid fd exhaustion under load.

**Nomad:**
```hcl
config {
  ulimit {
    nofile = "65535:65535"
  }
}
```

**Kubernetes:** Set at the node level or via an init container — pod-level ulimits are not directly configurable in the pod spec.

### Recommended host sysctls

For hosts running the orchestrator under high concurrency, tune these kernel parameters:

| Sysctl | Value | Purpose |
|--------|-------|---------|
| `net.core.somaxconn` | `65535` | Pending connection queue size |
| `net.ipv4.ip_local_port_range` | `10000 65535` | Ephemeral port range for outbound S3 connections |
| `net.ipv4.tcp_tw_reuse` | `1` | Reuse TIME_WAIT sockets during connection churn |
| `net.ipv4.tcp_fin_timeout` | `15` | Faster socket reclamation after close |

### Go runtime: GOMEMLIMIT

Set `GOMEMLIMIT` to ~90% of the container's memory allocation so the Go garbage collector can make informed collection decisions. Without it, the runtime has no awareness of the container's memory ceiling, which can lead to excessive GC (wasted CPU) or insufficient GC (OOM kill).

```bash
# Example for a 1024 MB container
GOMEMLIMIT=922MiB
```

The default `GOGC=100` provides smooth GC behavior with `GOMEMLIMIT` set. Setting `GOGC=off` minimizes GC CPU overhead but risks latency spikes from large collections — only use it after profiling your workload.

## Backend HTTP Transport

Each S3 backend gets a dedicated `http.Transport` with tuned connection pool settings. These are not configurable via YAML -- they are compiled defaults sized for the orchestrator's concurrent workload (rebalancer, replicator, parallel PUTs/GETs).

| Setting | Value | Purpose |
|---------|-------|---------|
| `MaxIdleConns` | 100 | Idle connection pool size per backend |
| `MaxIdleConnsPerHost` | 100 | Matches MaxIdleConns since each transport talks to one host |
| `MaxConnsPerHost` | 200 | Hard cap on active connections per backend (prevents unbounded growth during outages) |
| `IdleConnTimeout` | 60s | Recycles idle connections, forcing fresh DNS on reconnect |
| `KeepAlive` | 30s | TCP keepalive probes to detect stale connections |
| `DialTimeout` | 10s | Connection establishment timeout |
| `TLSHandshakeTimeout` | 10s | TLS negotiation timeout |
| `ResponseHeaderTimeout` | 30s | Time to read response headers from a backend |

### DNS Freshness

The 60-second `IdleConnTimeout` addresses DNS staleness for endpoints resolved via Consul DNS or load balancers. When an idle connection is discarded, the next request triggers a fresh DNS lookup, allowing the orchestrator to follow backend endpoint changes without restarts.

### Buffer Pool

All streaming operations (GET proxy, PUT body buffering, CopyObject, multipart assembly, UI downloads) use a shared `sync.Pool` of 32 KB byte buffers via `io.CopyBuffer` instead of `io.Copy`. This eliminates per-call buffer allocations that create GC pressure under high concurrency.

The CopyObject and multipart assembly streaming paths additionally wrap `io.PipeWriter` in pooled 64 KB `bufio.Writer` buffers, batching small writes to reduce syscall frequency on these background operations. Both pools are sized automatically and require no configuration.

## Routing Strategy

```yaml
routing_strategy: "pack"  # or "spread"
```

### pack (default)

Fills backends in config order. When the first backend reaches its quota, writes overflow to the next.

**Best for:**
- Stacking free-tier allocations (fill one provider, then the next)
- Cost optimization (use the cheapest backend first)
- Environments where you want predictable backend utilization

### spread

Places each write on the backend with the lowest utilization ratio (`(bytes_used + orphan_bytes) / bytes_limit`).

**Best for:**
- Distributing read load across backends (objects spread evenly)
- Maximizing aggregate throughput (parallel reads from multiple providers)
- Environments where all backends have similar performance characteristics

## Rebalancer Tuning

```yaml
rebalance:
  enabled: true
  strategy: "spread"       # or "pack"
  interval: "6h"
  batch_size: 100
  threshold: 0.1           # 10% utilization spread before triggering
  concurrency: 5
```

### Tradeoffs

| Setting | Higher Value | Lower Value |
|---------|-------------|-------------|
| `batch_size` | More objects moved per run, more API calls | Fewer moves, less API cost |
| `concurrency` | Faster rebalancing, more backend load | Slower but gentler on backends |
| `threshold` | Only rebalances when significantly uneven | Rebalances more aggressively |
| `interval` | Less frequent checks | More responsive to changes |

### Cost-Conscious Environments

If backends charge per API request (OCI, B2):

- Set `batch_size: 25-50` to limit API calls per rebalance run
- Set `concurrency: 2-3` to reduce parallel operations
- Set `threshold: 0.2` to only trigger when utilization is notably uneven
- Set `interval: "12h"` or `"24h"` to reduce frequency

### Performance-First Environments

If backends are local or have generous API allowances:

- Set `batch_size: 500` for faster convergence
- Set `concurrency: 10-20` for maximum parallelism
- Set `threshold: 0.05` for tighter balance

## Replication Worker

```yaml
replication:
  factor: 2
  worker_interval: "5m"
  batch_size: 50
  concurrency: 5
  unhealthy_threshold: "10m"
```

- `worker_interval` controls how often the worker checks for under-replicated objects. Lower intervals mean faster catch-up after writes but more database queries.
- `batch_size` limits objects processed per worker tick. Higher values catch up faster but create more backend I/O.
- `concurrency` controls how many objects are replicated in parallel within each tick. Higher values speed up convergence but increase backend load.
- `unhealthy_threshold` sets the grace period before the replicator treats copies on a circuit-broken backend as unavailable and creates replacements on healthy backends. Set higher to avoid churn during brief outages; set lower for faster redundancy recovery. Requires `backend_circuit_breaker.enabled: true`.
- The worker runs a catch-up pass on startup, so initial replication doesn't wait for the first interval.

For large backlogs (e.g., enabling replication on an existing dataset), temporarily increase `batch_size` to `500`, set `concurrency: 10`, and lower `worker_interval` to `"30s"`, then revert after catch-up.

## Cleanup Worker

```yaml
cleanup_queue:
  concurrency: 10
```

- `concurrency` controls how many orphaned objects are deleted in parallel per worker tick (default: 10). Higher values clear the queue faster during drain operations or sustained backend recovery, but increase backend load.

## Usage Flush

```yaml
usage_flush:
  interval: "30s"          # base flush interval
  adaptive_enabled: true   # shorten interval near limits
  adaptive_threshold: 0.8  # 80% of limit triggers fast flush
  fast_interval: "5s"      # interval when near limits
```

The orchestrator tracks API requests, egress, and ingress in memory and periodically flushes to the database. This is a tradeoff between accuracy and database load:

- **Lower `interval`**: more accurate usage tracking, more database writes
- **Higher `interval`**: less database load, but usage enforcement has more lag
- **Adaptive flushing** automatically switches to `fast_interval` when any backend exceeds `adaptive_threshold` of its limits. This keeps enforcement tight near limits without paying the cost of frequent flushes during normal operation.

For environments without usage limits, the flush still runs (it powers the dashboard and metrics) but accuracy is less critical. The 30-second default is fine.

### Redis Shared Counters

In multi-instance deployments, each instance maintains independent counters by default. Redis replaces per-instance atomics with shared counters, eliminating the cross-instance blind spot:

```yaml
redis:
  address: "redis.example.com:6379"
  key_prefix: "s3orch"
  failure_threshold: 3
  open_timeout: "15s"
```

Redis adds ~0.2ms per write operation (pipelined `INCRBY`) and ~0.2ms per read (`GET` pipeline). For most workloads the latency is negligible compared to backend S3 calls. If Redis becomes unavailable, the circuit breaker falls back to local counters automatically — no manual intervention needed.

**When to use Redis:**
- Running 2+ instances with usage limits enabled
- High write throughput where the 30s flush gap could cause meaningful overshoot

**When Redis is unnecessary:**
- Single instance deployments
- No usage limits configured
- Low throughput where the flush interval provides sufficient accuracy

## Backend Circuit Breaker

```yaml
backend_circuit_breaker:
  enabled: true
  failure_threshold: 5   # consecutive failures before opening (default: 5)
  open_timeout: "5m"     # delay before probing recovery (default: 5m)
```

Per-backend circuit breakers stop sending traffic to a backend after `failure_threshold` consecutive errors (expired credentials, network failures, provider outages). The circuit opens immediately — no timeout waiting — and all subsequent requests route around the failed backend. After `open_timeout`, the next organic request is allowed through as a probe.

### Tuning Guidelines

| Setting | Higher Value | Lower Value |
|---------|-------------|-------------|
| `failure_threshold` | More tolerant of transient errors, slower to open | Faster detection, but may trip on brief hiccups |
| `open_timeout` | Less probing traffic, slower recovery detection | Faster recovery, more probe requests to a potentially broken backend |

### Provider-Specific Recommendations

| Provider | Threshold | Timeout | Notes |
|----------|-----------|---------|-------|
| AWS S3 / R2 | `5` | `5m` | Defaults work well; outages are rare |
| OCI Object Storage | `3` | `5m` | OCI can return auth errors in bursts during credential rotation |
| Backblaze B2 | `5` | `5m` | B2 maintenance windows are short |
| MinIO (local) | `3` | `30s` | Local failures are usually configuration issues — probe quickly |

### When to Enable

- **Multi-backend with replication** — highly recommended. Reads fail over to replicas on healthy backends, writes route to other backends, and the replication worker creates replacement copies after a sustained outage (`replication.unhealthy_threshold`). A single backend failure becomes invisible to clients.
- **Single backend** — less useful. The circuit opens but there's nowhere to fail over to. Requests return `ErrBackendUnavailable` instead of timing out, which is still an improvement (faster failure).
- **Cost-sensitive environments** — the probe request after `open_timeout` is a real S3 API call. With a 5-minute timeout, that's at most 12 probes/hour to a down backend. Health-aware replication also creates additional copies during sustained outages, which consume backend I/O and storage.

### Monitoring

- `s3o_circuit_breaker_state{name="<backend>"}` — 0=closed, 1=open, 2=half-open
- `s3o_circuit_breaker_transitions_total{name="<backend>"}` — state change counter

A sustained `state=1` for a backend means it's been unreachable. Check the backend's credentials and connectivity.

## Rate Limiter Memory

```yaml
rate_limit:
  enabled: true
  requests_per_sec: 100
  burst: 200
  cleanup_interval: "1m"   # how often stale entries are evicted (default: 1m)
  cleanup_max_age: "5m"    # entries not seen within this window are evicted (default: 5m)
```

The per-IP rate limiter stores a token bucket for every unique client IP. A background goroutine sweeps the map every `cleanup_interval` and removes entries not seen within `cleanup_max_age`.

### Memory under attack

Under sustained high-cardinality traffic (e.g., DDoS with spoofed source IPs), the map can accumulate up to `cleanup_max_age / cleanup_interval` sweeps' worth of unique IPs before eviction catches up. Each entry is roughly 100 bytes, so 1 million unique IPs ≈ 100 MB.

To limit memory growth under attack:

- Lower `cleanup_max_age` to `1m` or `2m` — entries are evicted faster at the cost of re-creating limiters for legitimate clients who pause briefly.
- Lower `cleanup_interval` to `30s` — sweeps run more frequently, keeping the high-water mark lower.

### Monitoring

- `s3o_rate_limit_rejections_total` — counter of rejected requests. A sustained non-zero rate means the limiter is actively throttling traffic.

## Tracing Sample Rate

The `telemetry.sample_rate` controls what fraction of requests generate OpenTelemetry trace spans exported to the collector (Tempo, Jaeger, etc.). This does not affect Prometheus metrics or structured logs — only distributed traces.

```yaml
telemetry:
  sample_rate: 0.1   # 10% of requests traced
```

| Workload | Recommended Rate | Rationale |
|----------|-----------------|-----------|
| Development | `1.0` | Trace every request for debugging |
| Staging | `0.1` | Enough to see patterns; 10x less volume |
| Production < 100 RPS | `0.1` | ~10 traces/sec is manageable |
| Production 100–1000 RPS | `0.01` | ~1–10 traces/sec |
| Production > 1000 RPS | `0.001–0.01` | Reduce collector storage and CPU |

At `1.0` with 1000 RPS, the trace collector receives ~1000 spans/sec per request hop (multiple spans per request: HTTP → manager → backend). This can generate gigabytes of trace data per day and significant CPU overhead for serialization and export.

Prometheus metrics always capture 100% of requests regardless of sample rate — use metrics for alerting and dashboards, traces for debugging specific requests.

## Load Testing

The `loadtest/` directory contains tools for benchmarking the orchestrator under realistic S3 traffic. All tools handle SigV4 authentication automatically.

### Tools

**vegeta (Go)** — constant-rate latency profiling. Best for answering "what is P99 latency at N requests/second?"

**k6** — scenario-based workflow simulation. Best for answering "how does the system behave under realistic mixed traffic patterns?"

### Quick Start

Start the demo environment, then run the Make targets:

```bash
make nomad-demo   # or make kubernetes-demo

# Constant-rate PUT throughput
make loadtest-put LOADTEST_RATE=200 LOADTEST_SIZE=4096

# Constant-rate GET latency (pre-seeds objects first)
make loadtest-get LOADTEST_RATE=500 LOADTEST_SEED=1000

# Mixed PUT/GET workload
make loadtest-mixed LOADTEST_RATE=300 LOADTEST_DURATION=2m

# k6 burst test (admission control / load shedding)
make loadtest-burst

# k6 mixed CRUD workflow
make loadtest-k6
```

The vegeta targets accept these variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `LOADTEST_RATE` | `100` | Requests per second |
| `LOADTEST_DURATION` | `30s` | Test duration |
| `LOADTEST_SIZE` | `1024` | Object size in bytes |
| `LOADTEST_SEED` | `100` | Objects to pre-upload for GET/mixed |
| `LOADTEST_WORKERS` | `10` | Concurrent workers |
| `LOADTEST_ENDPOINT` | `http://localhost:9000` | S3 endpoint |
| `LOADTEST_BUCKET` | `photos` | Target bucket |

### Interpreting Results

**vegeta output:**

```
Requests      [total, rate, throughput]         3000, 100.03, 100.02
Latencies     [min, mean, 50, 90, 95, 99, max]  3.2ms, 4.2ms, 4.1ms, 4.8ms, 5.1ms, 6.6ms, 12.3ms
Success       [ratio]                           100.00%
Status Codes  [code:count]                      200:3000
```

- **P99 latency** is the most important number for production sizing. If P99 exceeds your SLA target, reduce the request rate or add instances.
- **Success ratio** below 100% means requests were rejected (429 rate limit or 503 admission control). Check `Status Codes` to see which.
- **Throughput** vs **rate**: if throughput is significantly lower than the configured rate, the server cannot keep up.

**k6 output:**

- `put_success` / `get_success` — per-operation success rates
- `shed_503` (burst test) — number of requests rejected by admission control
- `http_req_duration` — latency percentiles across all operations

### What to Watch in Grafana

While load tests run, the Grafana dashboard shows system behavior in real time:

- **Quota & Storage** — per-backend utilization during writes
- **Request Performance** — latency percentiles, status code distribution, request rate
- **Circuit Breaker** — backend health state transitions under load
- **Replication** — pending replica count and copy duration after write bursts
- **Cleanup Queue** — orphaned objects queued for cleanup after failures

### Baseline Workflow

To compare before/after a code change:

1. Start clean: `make clean && make nomad-demo`
2. Run the baseline: `make loadtest-put LOADTEST_RATE=500 LOADTEST_DURATION=1m`
3. Save the output (latencies, throughput, success ratio)
4. Apply the code change, rebuild: `make clean && make nomad-demo`
5. Run the same test with identical parameters
6. Compare P50/P95/P99 latencies and throughput

### Sizing Guidelines

These reference numbers were measured on a single instance with three local MinIO backends, replication factor 2, and AES encryption enabled:

| Workload | Rate | P99 Latency | CPU | Notes |
|----------|------|-------------|-----|-------|
| PUT 1KB | 100/s | ~7ms | ~50% | Steady with replication spikes |
| GET 1KB | 100/s | ~3ms | ~50% | Includes decryption |
| Mixed 1KB | 300/s | ~8ms | ~140% | 50/50 PUT/GET |
| Burst (100 VUs) | ~280/s | ~96ms | 150-200% spikes | k6 scenario, all PUTs |

CPU percentages are relative to the container's CPU allocation. Memory remained flat across all tests at 1KB object sizes — the streaming architecture avoids buffering objects in memory. Larger objects (1MB+) will increase memory usage proportionally to concurrency.
