# S3 Orchestrator Load Tests

Constant-rate and scenario-based load testing tools for the S3 API with SigV4 authentication.

## Quick Start

Start the demo environment, then use the Make targets from the repository root:

```bash
make nomad-demo   # or make kubernetes-demo

make loadtest-put                                          # 100 PUT/s, 30s, 1KB
make loadtest-get LOADTEST_SEED=1000                       # 100 GET/s, 1000 pre-seeded objects
make loadtest-mixed LOADTEST_RATE=300 LOADTEST_DURATION=2m # 300 req/s mixed PUT/GET
make loadtest-listobjects LOADTEST_SEED=10000              # 100 ListObjectsV2/s against 10k pre-seeded keys
make loadtest-burst                                        # k6 burst to 100 VUs
make loadtest-k6                                           # k6 mixed CRUD workflow
```

All vegeta targets accept these variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `LOADTEST_RATE` | `100` | Requests per second |
| `LOADTEST_DURATION` | `30s` | Test duration (per size in sweep mode) |
| `LOADTEST_SIZE` | `1024` | Object size in bytes (single-run mode) |
| `LOADTEST_SIZES` | (unset) | Comma-separated sizes for sweep mode (e.g. `1024,1048576,104857600`); overrides `LOADTEST_SIZE` |
| `LOADTEST_SEED` | `100` | Objects to pre-upload for GET/mixed (per size in sweep mode) |
| `LOADTEST_WORKERS` | `10` | Concurrent workers |
| `LOADTEST_ENDPOINT` | `http://localhost:9000` | S3 endpoint |
| `LOADTEST_BUCKET` | `photos` | Target bucket |
| `LOADTEST_OUTPUT_JSON` | (unset) | Path to write structured per-size results matrix |
| `LOADTEST_LIST_PREFIX` | `loadtest/` | Prefix for the listobjects scenario |
| `LOADTEST_LIST_MAX_KEYS` | `1000` | `max-keys` query parameter for the listobjects scenario |
| `LOADTEST_MPU_CONCURRENCY` | `10` | Concurrent VUs for the multipart scenario |
| `LOADTEST_MPU_PART_COUNT` | `5` | Parts per multipart upload |
| `LOADTEST_MPU_PART_SIZE` | `5242880` | Per-part size in bytes (5 MiB minimum) |

### List performance

`loadtest-listobjects` benchmarks `ListObjectsV2` latency against a
pre-seeded namespace. Vary `LOADTEST_SEED` across runs (e.g. 10k, 100k,
1M) to characterise how list latency scales with object count:

```bash
make loadtest-listobjects LOADTEST_SEED=10000   LOADTEST_OUTPUT_JSON=/tmp/list-10k.json
make loadtest-listobjects LOADTEST_SEED=100000  LOADTEST_OUTPUT_JSON=/tmp/list-100k.json
make loadtest-listobjects LOADTEST_SEED=1000000 LOADTEST_OUTPUT_JSON=/tmp/list-1m.json
```

### Saturation-find ramp

`-ramp-to` / `-ramp-step` drive the scenario at increasing rates
until the error rate exceeds `-ramp-error-threshold`. Output includes
every step plus a `saturation_rps` field marking the rate at which
saturation was first observed:

```bash
./loadtest/s3-loadtest \
  -op mixed -rate 100 -ramp-to 2000 -ramp-step 200 \
  -ramp-error-threshold 0.05 \
  -duration 30s -seed 500 \
  -output-json saturation.json
```

`-ramp-to` and `-sizes` are mutually exclusive (one swept dimension
per invocation).

### Cache-cold testing

`-cache-flush-before` POSTs to `/admin/api/cache/flush` before each
scenario step so cache-cold runs are not contaminated by previous
steps' warm hits. Requires an admin token (`-admin-token` flag or
`S3O_ADMIN_TOKEN` env var). 503 from the flush endpoint is treated
as success - it just means the orchestrator has caching disabled,
not that the call failed.

### Concurrent multipart

`loadtest-multipart` runs a k6 script that drives `CONCURRENCY`
parallel multipart uploads, each with `PART_COUNT` parts of
`PART_SIZE` bytes. Custom metrics report per-stage success rate and
latency:

```bash
make loadtest-multipart LOADTEST_MPU_CONCURRENCY=50 \
  LOADTEST_MPU_PART_COUNT=5 LOADTEST_MPU_PART_SIZE=5242880
```

The k6 SigV4 helper in `multipart.js` canonicalises query parameters
correctly (unlike the simpler `mixed.js` helper), so multipart's
`?uploads`, `?partNumber=N&uploadId=X`, and `?uploadId=X` URLs sign
without 403s.

### Sweep mode

Setting `LOADTEST_SIZES` runs the same scenario at each size in order
and emits a Markdown table summarising P50/P95/P99 latency, throughput
(req/s and MB/s), and error rate per size:

```bash
make loadtest-put LOADTEST_SIZES=1024,1048576,104857600 LOADTEST_OUTPUT_JSON=results.json
```

The JSON file captures the same matrix plus the host hardware
fingerprint (OS, arch, CPU count, Go version) and the static scenario
inputs, so a result file remains interpretable when read months later.

## Tools

### Vegeta (Go) — Steady-rate latency profiling

A Go program that uses [vegeta](https://github.com/tsenart/vegeta) as a library with SigV4-signed requests. Built automatically by the Make targets.

**Direct usage:**

```bash
cd loadtest
go build -o s3-loadtest .

./s3-loadtest -op put -rate 200 -duration 1m -size 4096
./s3-loadtest -op get -rate 500 -duration 1m -seed 1000
./s3-loadtest -op mixed -rate 300 -duration 2m -seed 500
```

| Flag | Default | Description |
|------|---------|-------------|
| `-op` | `put` | `put`, `get`, `mixed` (50/50 PUT/GET), or `listobjects` |
| `-rate` | `100` | Requests per second |
| `-duration` | `30s` | Test duration |
| `-size` | `1024` | Object size in bytes |
| `-workers` | `10` | Concurrent workers |
| `-seed` | `100` | Objects to pre-upload for `get`/`mixed` |
| `-endpoint` | `http://localhost:9000` | S3 endpoint |
| `-access-key` | `photoskey` | AWS access key ID |
| `-secret-key` | `photossecret` | AWS secret access key |
| `-bucket` | `photos` | Target bucket |
| `-region` | `us-east-1` | AWS region for SigV4 |
| `-sizes` | (unset) | Comma-separated object sizes for sweep mode; overrides `-size` |
| `-output-json` | (unset) | Path to write structured per-size results matrix |
| `-list-prefix` | `loadtest/` | Prefix for the listobjects scenario |
| `-list-max-keys` | `1000` | `max-keys` query parameter for the listobjects scenario |
| `-ramp-to` | `0` | Saturation-find: ramp from `-rate` up to this rate; stops when error rate exceeds `-ramp-error-threshold` (0 disables ramp) |
| `-ramp-step` | `100` | Rate increment per ramp step |
| `-ramp-error-threshold` | `0.05` | Error rate threshold (0..1) for ramp termination |
| `-cache-flush-before` | `false` | POST `/admin/api/cache/flush` before each scenario step (requires `-admin-token`) |
| `-admin-token` | (unset) | Admin token for cache-flush calls (or `S3O_ADMIN_TOKEN` env var) |

### k6 — Scenario-based workflow simulation

JavaScript scripts for [k6](https://k6.io/) that simulate realistic user workflows with SigV4 signing.

**Install k6:** https://grafana.com/docs/k6/latest/set-up/install-k6/

#### mixed.js — CRUD workflow

Each virtual user uploads a batch of objects, downloads a random subset, then deletes everything. Ramps up to 10 VUs, holds for 30 seconds, ramps down.

```bash
k6 run k6/mixed.js
k6 run k6/mixed.js --env OBJECT_COUNT=50 --env OBJECT_SIZE=8192
```

| Env var | Default | Description |
|---------|---------|-------------|
| `S3_ENDPOINT` | `http://localhost:9000` | S3 endpoint |
| `S3_BUCKET` | `photos` | Target bucket |
| `AWS_ACCESS_KEY_ID` | `photoskey` | Access key |
| `AWS_SECRET_ACCESS_KEY` | `photossecret` | Secret key |
| `AWS_REGION` | `us-east-1` | SigV4 region |
| `OBJECT_COUNT` | `20` | Objects per VU iteration |
| `OBJECT_SIZE` | `1024` | Object size in bytes |

#### burst.js — Admission control / load shedding

Spikes from 0 to 100 concurrent VUs in 2 seconds and holds for 20 seconds. Designed to trigger `max_concurrent_requests` limits and `503 SlowDown` responses.

```bash
k6 run k6/burst.js
k6 run k6/burst.js --env PEAK_VUS=200 --env OBJECT_SIZE=65536
```

| Env var | Default | Description |
|---------|---------|-------------|
| `PEAK_VUS` | `100` | Peak virtual users |
| `OBJECT_SIZE` | `4096` | Object size in bytes |

The `shed_503` counter in the output shows how many requests were rejected.
