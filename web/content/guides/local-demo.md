---
title: "Nomad/k8s Full Stack Demo"
description: "Run the full demo stack locally: three s3-orchestrator instances behind Traefik, three MinIO backends, PostgreSQL and Redis, on Nomad or Kubernetes."
weight: 1
---


This guide walks through running the S3 Orchestrator demo environment on your local machine. The demo stands up three s3-orchestrator instances behind Traefik on port 9000 (set `INSTANCES=1` for one), sharing PostgreSQL and Redis, with three MinIO backends and a complete observability stack (Prometheus, Grafana, Tempo, Loki). Two orchestrators are available: **Nomad** and **Kubernetes** (via k3d). Both expose the same functionality - pick whichever you prefer.

## Prerequisites

Both demos require:

- [Docker](https://docs.docker.com/get-docker/) - the image is built from source with `docker build`
- [AWS CLI](https://docs.aws.amazon.com/cli/latest/userguide/install-cliv2.html) (or any S3-compatible client)
- `jq`

**Nomad demo** additionally requires:

- [Nomad](https://developer.hashicorp.com/nomad/install) - runs in `-dev` mode, no cluster setup needed
- `curl`

**Kubernetes demo** additionally requires:

- [k3d](https://k3d.io/) - lightweight Kubernetes in Docker
- [kubectl](https://kubernetes.io/docs/tasks/tools/)
- [helm](https://helm.sh/docs/intro/install/)

## Starting the Demo

Clone the repository and run the appropriate Makefile target:

```bash
git clone https://github.com/afreidah/s3-orchestrator.git
cd s3-orchestrator

# Nomad
make nomad-demo

# Kubernetes
make kubernetes-demo
```

The script builds the container image from source, runs the backing services in the chosen orchestrator - as Nomad jobs, or as workloads in the k3d cluster - deploys s3-orchestrator alongside them, and waits for the health check to pass. When everything is ready you will see a summary like this:

```
========================================
  S3 Orchestrator is running in Nomad
========================================

  S3 API:     http://localhost:9000  (Traefik over 3 instances)
  Dashboard:  http://localhost:9000/ui/
  Health:     http://localhost:9000/health
  Traefik:    http://localhost:8081/dashboard/
  Grafana:    http://localhost:13000
  Prometheus: http://localhost:19090
  Tempo:      http://localhost:3200
  Nomad UI:   http://localhost:4646
  Metrics and pprof, one listener per instance:
    http://127.0.0.1:24137/metrics  (alloc 3f2a9c1d)
    ...

  Root keypair - the only credential, for the dashboard login, the
  TUI, and the admin API:
    access key: ...
    secret key: ...

    export S3O_ADMIN_ADDR=http://localhost:9000
    export S3O_ACCESS_KEY_ID=...
    export S3O_SECRET_ACCESS_KEY=...
    s3-orchestrator admin status
    s3-orchestrator tui
```

The summary goes on to show a test upload, the `perf` identity `make perf` signs as, and the teardown command.

## What Is Running

The demo starts several services behind the scenes:

| Service | Port | Purpose |
|---------|------|---------|
| **Traefik** | 9000, 8081 | Routes S3, dashboard and admin traffic across the instances; its own dashboard on 8081 |
| **S3 Orchestrator** | behind 9000 | Three instances by default; each serves `/metrics` and pprof on its own listener (container port 9001) |
| **MinIO 1** | 19000 | First storage backend (10 GB quota) |
| **MinIO 2** | 19002 | Second storage backend (10 GB quota) |
| **MinIO 3** | 19004 | Third storage backend (10 GB quota) |
| **PostgreSQL** | 15432 | Metadata database |
| **Redis** | 16379 (Nomad) | Shared usage counters across the instances |
| **Prometheus** | 19090 | Metrics collection (10 s scrape interval) |
| **Grafana** | 13000 | Dashboard, log exploration, trace viewer |
| **Tempo** | 3200 | Distributed tracing backend |
| **Loki** | 3100 | Log aggregation |
| **Alloy** | - | Log collector (ships container logs to Loki) |

The Nomad demo also starts a **Nomad dev agent** on port 4646. The Kubernetes demo creates a **k3d cluster** with a single node; there, MinIO, PostgreSQL, Redis and Loki are only reachable inside the cluster, and the orchestrator, Grafana, Prometheus and Tempo are published on the ports above.

### Demo Configuration

Both demos are pre-configured with:

- **Three backends** - `minio-1`, `minio-2`, and `minio-3`, each with a 10 GB quota
- **Replication factor 2** - every write places both copies itself (`write_path.parallel_copies`), and the replicator repairs any it missed
- **Encryption enabled** - all objects are AES-encrypted at rest
- **Circuit breaker** - backends are automatically taken out of rotation after 3 consecutive failures
- **Full telemetry** - 100% trace sampling, structured JSON logging with trace correlation
- **Admin UI** - enabled, logged into with the root keypair the demo prints when it finishes

Both demos render the same configuration, `deploy/local/config.yaml`, and use `spread` routing, which places writes by utilization ratio across the three backends.

## Uploading Files

Use the AWS CLI to upload objects through the orchestrator:

```bash
# Configure credentials (match the demo bucket config)
export AWS_ACCESS_KEY_ID=photoskey
export AWS_SECRET_ACCESS_KEY=photossecret

# Upload a single file
aws --endpoint-url http://localhost:9000 s3 cp /etc/hostname s3://photos/test.txt

# Upload a directory
aws --endpoint-url http://localhost:9000 s3 cp ~/Pictures s3://photos/pictures/ --recursive

# List objects
aws --endpoint-url http://localhost:9000 s3 ls s3://photos/ --recursive

# Download a file
aws --endpoint-url http://localhost:9000 s3 cp s3://photos/test.txt /tmp/test.txt

# Delete a file
aws --endpoint-url http://localhost:9000 s3 rm s3://photos/test.txt
```

Any S3-compatible client works - `s3cmd`, `rclone`, `mc` (MinIO Client), or your application's S3 SDK.

## Admin Dashboard

Open [http://localhost:9000/ui/](http://localhost:9000/ui/) and log in with the root keypair printed in the demo's summary under "Root keypair": the access key and secret key go in the two login fields.

The dashboard provides a real-time overview of the system:

- **Storage Summary** - total bytes used and capacity across all backends with a progress bar
- **Backends Table** - per-backend quota usage, object count, and active multipart uploads
- **Monthly Usage** - API requests, egress, and ingress per backend
- **Objects** - interactive directory tree; click folders to expand, click files to download
- **Configuration** - virtual buckets, routing strategy, replication factor, and rate limit status
- **Logs** - recent structured log output, filterable by severity level

### Management Actions

The dashboard also exposes management operations:

- **Upload** - upload files or entire folders via the browser; supports multi-file selection and preserves directory structure
- **Delete** - remove individual objects or batch-delete by prefix
- **Rebalance** - trigger an on-demand rebalance to redistribute objects across backends according to `rebalance.strategy`
- **Sync** - import pre-existing objects from a backend's S3 bucket into the orchestrator database (useful when adding a new backend that already has data)

## Grafana Dashboard

Open [http://localhost:13000](http://localhost:13000) (no login required - anonymous admin is enabled).

The pre-loaded **S3 Orchestrator** dashboard is available under **Dashboard** in the left sidebar. It contains 50+ panels organized into sections. Upload some files and watch the panels update in real time.

### Overview

The top of the dashboard shows build info, total request count, current request rate, error rate, and inflight requests at a glance. Below that, the **Quota & Storage** section tracks per-backend quota usage over time, total object count, and available space.

![Dashboard Overview](/images/demo-grafana-overview.png)

### Request Performance

Detailed HTTP-level metrics: request rate broken down by method, duration percentiles (p50/p95/p99), request and response body sizes, and HTTP status code distribution. These panels help identify slow operations and error patterns under load.

![Request Performance](/images/demo-grafana-requests.png)

### Manager Operations

The backend and manager operations panels show request rate and duration at the storage layer - below the HTTP handler and above the individual S3 backends. This section also displays the circuit breaker status for each backend and the database, replication activity (pending replicas, copies created, errors, duration), rebalancer runs, the cleanup queue depth, and the cleanup DLQ depth (`s3o_cleanup_dlq_depth`) - a non-zero DLQ means at least one unrecoverable orphan needs operator action.

![Manager Operations](/images/demo-grafana-manager.png)

### Drain, Lifecycle & Encryption

The Drain & Lifecycle and Encryption sections cover drain operations (active drains, objects and bytes moved), lifecycle rule enforcement (objects deleted, failed deletions, rule evaluation runs), and encryption metrics (encrypt/decrypt operations, errors, key rotation events).

![Drain, Lifecycle & Encryption](/images/demo-grafana-lifecycle.png)

## Prometheus Metrics

Each instance serves `/metrics` on its own metrics listener (container port 9001), not on the S3 port; the Nomad demo's summary lists each instance's address, and on Kubernetes `kubectl port-forward pod/<name> 9001` reaches one. Prometheus scrapes them every 10 seconds. To explore raw metrics, go to **Explore** in Grafana, select the **Prometheus** datasource, and browse the available metrics. All metrics are prefixed with `s3o_`.

![Prometheus Metrics](/images/demo-prometheus.png)

## Distributed Tracing

Every S3 request generates an OpenTelemetry trace that flows to Tempo. Traced operations include S3 API requests, replication copies, rebalance moves, multipart cleanup, and cleanup queue processing.

In Grafana, go to **Explore** and select the **Tempo** datasource. The default view shows recent traces searchable by service name, duration, or status. Click any trace to see the full span waterfall - the request through the orchestrator, the backend storage operation, and any replication work it triggered.

![Tempo Explore](/images/demo-traces-explore.png)

Tempo also generates a **Service Graph** from the trace data, showing s3-orchestrator and its connections to the storage backends. Click **Service Graph** in the Tempo explore view to see it.

![Service Graph](/images/demo-traces-service-graph.png)

Click any node or edge in the service graph to jump to the underlying traces. Selecting an individual trace opens the span waterfall, showing the full request lifecycle - from the HTTP handler through backend storage operations and any replication work it triggered.

![Trace Detail](/images/demo-traces.png)

## Log Exploration

Structured JSON logs are collected by Alloy and shipped to Loki. To explore:

1. In Grafana, go to **Explore**
2. Select the **Loki** datasource
3. Run `{service_name="s3-orchestrator"}` to see all logs
4. Filter by level: `{service_name="s3-orchestrator"} |= "ERROR"`
5. Search for audit events: `{service_name="s3-orchestrator"} |= "audit"`

Logs include structured fields like `event`, `request_id`, `backend`, `key`, `size`, and `duration` for audit trail and debugging.

![Log Exploration in Grafana](/images/demo-logs.png)

## Trace-to-Log Correlation

Traces and logs are linked bidirectionally:

- **Logs to traces** - log entries that contain a `trace_id` field show a **View Trace** link. Click it to jump directly to the full trace in Tempo.
- **Traces to logs** - when viewing a trace in Tempo, click **View logs** on any span to see the corresponding log entries in Loki for that trace.

This lets you start from either direction - find a slow trace and see what the application logged, or find an error log and see the full request lifecycle.

![Trace-to-Log Correlation](/images/demo-log-trace-correlation.png)

## Experimenting

Here are some things to try:

### Watch replication in action

Upload a file and check the Backends Table in the admin dashboard. The write places both copies itself (`write_path.parallel_copies`), so the object shows on two backends as soon as the upload returns. Lower `write_path.parallel_copies.max_in_flight` in `deploy/local/config.yaml` to watch writes fall back to a single copy that the replicator then repairs within its 20-second interval.

### Trigger a rebalance

A rebalance moves objects to match `rebalance.strategy` (separate from `routing_strategy`), and only when backend utilization differs by more than `rebalance.threshold` (0.7 in the demo). If objects are already where the strategy wants them, rebalance is a no-op.

To see it in action:

1. In `deploy/local/config.yaml`, set `routing_strategy: "pack"` and lower `rebalance.threshold`, then restart the demo
2. Upload several files - `pack` routing places them on `minio-1` first
3. Click **Rebalance** in the admin dashboard
4. Objects migrate from `minio-1` to `minio-2` and `minio-3` until utilization evens out
5. Watch the **Rebalancer** section in the Grafana dashboard to see objects and bytes moving

### Test circuit breaker behavior

1. Stop one of the MinIO backends:
   - Nomad: `docker stop $(docker ps -qf name=minio-1-)`
   - Kubernetes: `kubectl --context k3d-s3-orchestrator-demo -n demo scale deployment/minio-1 --replicas=0`
2. Upload a file - the orchestrator routes to the healthy backend
3. Check the **Circuit Breaker** panel in Grafana to see the state transition
4. Bring MinIO back. Nomad restarts the task on its own after its restart delay, usually at a new container address, so about 30 seconds later the orchestrator instances restart once to pick it up; on Kubernetes, scale it back with `--replicas=1`. The backend keeps its data either way.
5. The circuit breaker resets after 15 seconds and the backend rejoins the pool

### Explore encryption

Objects are encrypted transparently. To verify:

1. Upload a file through the orchestrator
2. Try reading it directly from MinIO at `http://localhost:19000` - the content is encrypted ciphertext (on Kubernetes, first run `kubectl --context k3d-s3-orchestrator-demo -n demo port-forward svc/minio-1 19000:9000`)
3. Read it back through the orchestrator at `http://localhost:9000` - it is decrypted automatically

## Tearing Down

```bash
# Nomad
./deploy/nomad/local/demo.sh down

# Kubernetes
./deploy/kubernetes/local/demo.sh down
```

This stops the orchestrator and every backing service, and stops the Nomad dev agent or deletes the k3d cluster.
