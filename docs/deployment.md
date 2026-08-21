---
title: "Deployment"
linkTitle: "Deployment"
weight: 34
---

# Deployment


### Nomad and Kubernetes

Production-ready manifests for both platforms are in [`deploy/`](../deploy/). Each includes a local demo script that stands up a complete environment in one command:

```bash
make kubernetes-demo   # k3d cluster with docker-compose backing services
make nomad-demo        # Nomad dev agent with docker-compose backing services
```

See [`deploy/README.md`](../deploy/README.md) for production deployment instructions, Vault integration, TLS/mTLS configuration, and Ingress setup.

### Multi-instance deployment

By default, every instance runs both the HTTP API and all background workers (`--mode=all`). For larger deployments, the `--mode` flag separates these roles:

| Mode | HTTP API | Background workers | Use case |
|------|----------|-------------------|----------|
| `all` (default) | Yes | All 6 services | Single-instance or small deployments |
| `api` | Yes | Usage flush only | Scale-out API instances behind a load balancer |
| `worker` | Health + metrics only | All 6 services | Dedicated background processing |

```bash
# API instance — serves S3 requests, no background workers
s3-orchestrator -config config.yaml -mode api

# Worker instance — runs rebalancer, replicator, cleanup, lifecycle
s3-orchestrator -config config.yaml -mode worker
```

**How it works:**

- **API instances** serve S3 requests, the web UI, and rate limiting. They run the usage-flush service to avoid losing counters on restart, but skip all advisory-locked background tasks.
- **Worker instances** run all background services and expose `/health`, `/health/ready`, and `/metrics` for monitoring, but don't serve S3 traffic or the web UI.
- Background tasks that modify state (rebalancer, replicator, cleanup, lifecycle, multipart cleanup) use **PostgreSQL advisory locks** — only one instance cluster-wide executes each task per cycle. Running multiple worker instances is safe; extra instances simply skip cycles when the lock is held. The circuit breaker watchdog runs on all instances without a lock (it operates on per-instance circuit state).

**Recommended topology:** N `api` instances behind a load balancer + 1–2 `worker` instances for redundancy. All instances share the same config file and PostgreSQL database.

### Docker

```bash
# Local build
make build

# Multi-arch build and push to registry with version tag
make push VERSION=vX.Y.Z
```

The `VERSION` is baked into the binary via `-ldflags` and displayed in the web UI and `/health` endpoint. Use versioned tags (not `latest`) to avoid Docker layer caching issues on orchestration platforms.

```bash
docker run -d \
  --name s3-orchestrator \
  -v /path/to/config.yaml:/etc/s3-orchestrator/config.yaml \
  -p 9000:9000 \
  -e DB_PASSWORD=secret \
  -e OCI_ACCESS_KEY=... \
  -e OCI_SECRET_KEY=... \
  s3-orchestrator
```

The default entrypoint is `s3-orchestrator -config /etc/s3-orchestrator/config.yaml`. Mount your config file to that path, or override the command to use a different location.

Environment variables referenced in the config via `${VAR}` syntax are expanded at startup, so pass secrets as `-e` flags or via your orchestration platform's secret injection.

The `listen_addr` in your config determines which port the process binds to inside the container — make sure your `-p` mapping matches.

### Debian Package (Systemd)

Build a `.deb` package for bare-metal or VM deployments:

```bash
# Build for host architecture
make deb VERSION=X.Y.Z

# Build for both amd64 and arm64
make deb-all VERSION=X.Y.Z

# Build and validate with lintian
make deb-lint VERSION=X.Y.Z
```

Install and configure:

```bash
sudo dpkg -i s3-orchestrator_X.Y.Z_amd64.deb

# Edit the config — set database, backends, buckets
sudo vim /etc/s3-orchestrator/config.yaml

# Set secrets as environment variables (referenced via ${VAR} in config)
sudo vim /etc/default/s3-orchestrator

# Start the service
sudo systemctl start s3-orchestrator

# Check status
sudo systemctl status s3-orchestrator
sudo journalctl -u s3-orchestrator -f
```

The package installs:

| Path | Purpose |
|------|---------|
| `/usr/bin/s3-orchestrator` | Binary |
| `/etc/s3-orchestrator/config.yaml` | Configuration (conffile, preserved on upgrade) |
| `/etc/default/s3-orchestrator` | Environment variables for `${VAR}` expansion in config |
| `/usr/lib/systemd/system/s3-orchestrator.service` | Systemd unit |
| `/var/lib/s3-orchestrator/` | Data directory |

The systemd unit runs as a dedicated `s3-orchestrator` user with filesystem hardening (`ProtectSystem=strict`, `ProtectHome=yes`, `NoNewPrivileges=yes`). The service is enabled on install but not started automatically, allowing configuration before first start.

**Config reload** works via systemd:

```bash
sudo systemctl reload s3-orchestrator
```

This sends `SIGHUP` to the process, reloading bucket credentials, quota limits, rate limits, and rebalance/replication settings without downtime. See [Reloading configuration](#reloading-configuration) for details on what is and isn't reloadable.

**Uninstall:**

```bash
# Remove the package (preserves config and data)
sudo apt remove s3-orchestrator

# Remove everything including config, data, and system user
sudo apt purge s3-orchestrator

## Multi-instance deployment


Multiple orchestrator instances can safely share the same PostgreSQL database. Background tasks (rebalancer, replicator, cleanup queue, multipart cleanup) use PostgreSQL advisory locks to prevent concurrent execution across instances — if one instance holds the lock for a task, other instances skip that tick silently.

Request-serving paths (PutObject, GetObject, etc.) are stateless and work correctly with any number of instances behind a load balancer. The per-instance location cache is TTL-bounded and self-correcting. Rate limiting remains per-instance.

### Usage Counters

Without Redis, each instance tracks usage counters independently in memory and flushes to PostgreSQL at the configured interval (default 30s). Between flushes, instances cannot see each other's accumulated usage, which can allow quota overshoot under high throughput.

With Redis configured, all instances share the same usage counters via Redis `INCRBY`/`GET` operations. The baseline+delta formula stays the same (`DB baseline + counter + proposed`), but the counter lives in Redis instead of local memory, eliminating the cross-instance blind spot. When Redis is active, only one instance flushes counters to PostgreSQL (coordinated via advisory lock) since `GETSET` is a destructive read.

A circuit breaker monitors Redis health. If Redis becomes unavailable, the backend falls back to local in-memory counters automatically — same behavior as running without Redis. A background health probe PINGs Redis periodically and, on recovery, syncs local deltas back to Redis via an additive INCRBY pipeline before resuming shared operation. The entire local counter map is swapped atomically (single pointer swap) so no concurrent Add calls can lose deltas between the snapshot and the pipeline. Stale Redis keys from before the outage expire via TTL. Local counters are zeroed only after the pipeline commits, so a crash mid-recovery cannot lose deltas. The recovery is safe for concurrent execution by multiple instances since INCRBY is additive.

```yaml
redis:
  address: "redis.example.com:6379"
  password: "${REDIS_PASSWORD}"
  key_prefix: "s3orch"       # namespace for multi-tenant Redis
  failure_threshold: 3        # consecutive failures before fallback
  open_timeout: "15s"         # delay before probing recovery
```

Redis is optional. Without it, adaptive flushing still shortens the flush interval when any backend approaches a usage limit, improving enforcement accuracy.
