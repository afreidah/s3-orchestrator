# S3 Orchestrator Load Tests

Constant-rate and scenario-based load testing tools for the S3 API with SigV4 authentication.

## Quick Start

Start the demo environment, then use the Make targets from the repository root:

```bash
make nomad-demo   # or make kubernetes-demo

make loadtest-put                                          # 100 PUT/s, 30s, 1KB
make loadtest-get LOADTEST_SEED=1000                       # 100 GET/s, 1000 pre-seeded objects
make loadtest-mixed LOADTEST_RATE=300 LOADTEST_DURATION=2m # 300 req/s mixed PUT/GET
make loadtest-burst                                        # k6 burst to 100 VUs
make loadtest-k6                                           # k6 mixed CRUD workflow
```

All vegeta targets accept these variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `LOADTEST_RATE` | `100` | Requests per second |
| `LOADTEST_DURATION` | `30s` | Test duration |
| `LOADTEST_SIZE` | `1024` | Object size in bytes |
| `LOADTEST_SEED` | `100` | Objects to pre-upload for GET/mixed |
| `LOADTEST_WORKERS` | `10` | Concurrent workers |
| `LOADTEST_ENDPOINT` | `http://localhost:9000` | S3 endpoint |
| `LOADTEST_BUCKET` | `photos` | Target bucket |

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
| `-op` | `put` | `put`, `get`, or `mixed` (50/50 PUT/GET) |
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
