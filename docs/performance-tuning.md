# Performance Tuning

This guide covers configuration knobs that affect throughput, latency, and resource usage.

## Connection Pool Sizing

```yaml
database:
  max_conns: 10          # max pool connections (default: 10)
  min_conns: 5           # min idle connections (default: 5)
  max_conn_lifetime: "5m" # max connection age (default: 5m)
```

### Guidelines

- **Single instance, 1-2 backends**: the defaults (10 max, 5 min) are sufficient for most workloads.
- **Single instance, 3+ backends**: increase to `max_conns: 20` since each S3 operation involves at least one database query and concurrent requests multiply quickly.
- **Multi-instance deployment**: divide your PostgreSQL `max_connections` across instances. Each instance's `max_conns` should be `(max_connections - 20) / instance_count` (reserve 20 for superuser and monitoring connections).
- **High-throughput workloads** (1000+ requests/sec): increase `max_conns: 30-50` and monitor `pgx_pool_acquired_conns` / `pgx_pool_total_conns` in Prometheus.
- `min_conns` keeps idle connections warm. Set it to roughly half of `max_conns` to avoid connection setup latency on traffic spikes.
- `max_conn_lifetime` rotates connections to pick up DNS changes and distribute load across replicas. The 5-minute default works well in most environments.

## Admission Control

```yaml
server:
  max_concurrent_requests: 0  # 0 = unlimited (default)
```

Limits the total number of S3 requests being processed concurrently. When the limit is reached, new requests are rejected immediately with `503 SlowDown` instead of queueing and potentially exhausting backend connections and goroutines.

### When to set this

- **Default (0)**: no limit. Fine for low-traffic single-instance deployments.
- **Recommended starting point**: set to 2-3x your `database.max_conns` value. Every S3 operation needs at least one database query, so there's no benefit admitting more requests than the connection pool can serve.
- **High-traffic environments**: set based on your observed peak concurrency from `s3proxy_inflight_requests` metrics. Start with a value 50% above observed peak and adjust downward if you want tighter load shedding.

### Monitoring

- `s3proxy_admission_rejections_total` — counter of rejected requests. If this is consistently non-zero, either increase the limit or investigate why the server is saturated.
- `s3proxy_inflight_requests` — gauge of currently processing requests by method. Use this to find the right limit value.

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

Places each write on the backend with the lowest utilization ratio (`bytes_used / bytes_limit`).

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
```

- `worker_interval` controls how often the worker checks for under-replicated objects. Lower intervals mean faster catch-up after writes but more database queries.
- `batch_size` limits objects processed per worker tick. Higher values catch up faster but create more backend I/O.
- The worker runs a catch-up pass on startup, so initial replication doesn't wait for the first interval.

For large backlogs (e.g., enabling replication on an existing dataset), temporarily increase `batch_size` to `500` and lower `worker_interval` to `"30s"`, then revert after catch-up.

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

- **Multi-backend with replication** — highly recommended. Reads fail over to replicas on healthy backends, writes route to other backends. A single backend failure becomes invisible to clients.
- **Single backend** — less useful. The circuit opens but there's nowhere to fail over to. Requests return `ErrBackendUnavailable` instead of timing out, which is still an improvement (faster failure).
- **Cost-sensitive environments** — the probe request after `open_timeout` is a real S3 API call. With a 5-minute timeout, that's at most 12 probes/hour to a down backend.

### Monitoring

- `s3proxy_circuit_breaker_state{name="<backend>"}` — 0=closed, 1=open, 2=half-open
- `s3proxy_circuit_breaker_transitions_total{name="<backend>"}` — state change counter

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

- `s3proxy_rate_limit_rejections_total` — counter of rejected requests. A sustained non-zero rate means the limiter is actively throttling traffic.
