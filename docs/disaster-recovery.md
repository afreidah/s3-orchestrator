This guide covers failure scenarios and recovery procedures for the S3 Orchestrator.

## Architecture Context

The orchestrator has two required stateful components and one optional:

- **PostgreSQL** stores object locations, quota counters, usage stats, multipart state, and the cleanup queue. This is the source of truth for "which object lives on which backend."
- **Storage backends** (OCI, R2, S3, MinIO, etc.) hold the actual object data. These are independent and unaware of each other.
- **Redis** (optional) provides shared usage counters across instances. Not a data dependency — all authoritative data lives in PostgreSQL. See [Redis Failure](#redis-failure) below.

The orchestrator binary itself is stateless. Any instance with access to the database and backends can serve requests.

## PostgreSQL Failure

### What happens

When PostgreSQL becomes unreachable, the circuit breaker activates automatically:

- **Reads** enter degraded mode: the orchestrator broadcasts GET requests to all backends (or uses a short-lived cache of recent key-to-backend mappings if `circuit_breaker.cache_ttl` is configured). Objects found on any backend are returned normally.
- **Writes** return `503 Service Unavailable` because the orchestrator cannot record the object's location without the database.
- **Background workers** (rebalancer, replicator, usage flush, cleanup queue) pause until the database recovers.

The circuit breaker transitions back to healthy after successful database probes. No manual intervention is needed for recovery.

### Monitoring

Watch for these log messages:

```
{"level":"WARN","msg":"Circuit breaker opened","consecutive_failures":3}
{"level":"INFO","msg":"Circuit breaker closed","state":"healthy"}
```

The `/health` endpoint continues to return `200 OK` with `"status":"degraded"` during database outages, so the service stays in load balancer rotation for reads.

## Restoring PostgreSQL from Backup

1. **Restore the database** using your standard PostgreSQL backup procedure (pg_dump/pg_restore, WAL replay, etc.).

2. **Start the orchestrator** (or let it reconnect). Database migrations are embedded in the binary and applied automatically on startup via goose. Any missing migrations are applied in order.

3. **Reconcile metadata** if objects were written to backends while the database was down (e.g., via direct S3 API access). For each backend, run:

   ```bash
   s3-orchestrator sync \
     --config config.yaml \
     --backend <backend-name> \
     --bucket <virtual-bucket>
   ```

   The sync command scans the backend's S3 bucket and imports objects not already tracked in the database. Existing records are skipped.

4. **Verify** by checking the admin CLI:

   ```bash
   s3-orchestrator admin status
   ```

## Backend Permanently Unavailable

If a storage backend is permanently lost (provider shutdown, account deleted, etc.):

### With replication (factor >= 2)

Objects that have replicas on other backends continue to be served transparently. The orchestrator tries the failed backend, gets an error, and falls through to the next copy.

If backend circuit breakers are enabled and the outage lasts longer than `replication.unhealthy_threshold` (default: 10 minutes), the replication worker automatically creates replacement copies on healthy backends to maintain the configured replication factor. This restores full redundancy without manual intervention while the failed backend remains down.

To clean up, remove the backend's database records:

```bash
s3-orchestrator admin remove-backend <backend-name>
```

Then remove the backend from the config file and restart.

### Without replication (factor = 1)

Objects stored exclusively on the lost backend are unrecoverable from the orchestrator's perspective. The database records remain but point to a backend that no longer exists.

To clean up, remove the orphaned database records:

```bash
s3-orchestrator admin remove-backend <backend-name>
```

Then remove the backend from the config file and restart.

### Planned decommission

If a backend is still reachable but you want to decommission it, use the drain operation to migrate all objects to other backends first (no data loss):

```bash
# Start the drain — objects are migrated in the background
s3-orchestrator admin drain <backend-name>

# Monitor progress
s3-orchestrator admin drain-status <backend-name>

# Once complete, remove from config and restart
```

See the [Admin Guide](admin-guide.md#draining-a-backend) for the full drain workflow.

### Preventing data loss

Enable replication with `replication.factor: 2` or higher so every object exists on at least two backends. This is the primary defense against backend loss.

## Cleanup Queue Recovery

The cleanup queue tracks failed delete operations with exponential backoff (1 minute to 24 hours, max 10 attempts). If items get stuck:

```bash
# Check the queue
s3-orchestrator admin cleanup-queue

# The background worker processes items automatically every minute.
# If the queue is backed up, check the logs for persistent errors.
```

Items that exhaust all 10 retry attempts are logged and left in the queue for manual review.

## Multi-Instance Recovery

When running multiple orchestrator instances:

- **Advisory locks** in PostgreSQL ensure only one instance runs each background worker (rebalancer, replicator, cleanup queue, lifecycle). If an instance crashes, the lock is released and another instance picks up the work.
- **No split-brain risk** for writes because PostgreSQL transactions serialize object location records. Two instances writing the same key will both succeed, but only one location record wins (the database is the arbiter).
- **Startup catch-up**: the replication worker runs an immediate reconciliation pass on startup before entering its periodic loop. This handles any objects that fell behind while instances were down.

## Redis Failure

When Redis is configured for shared usage counters:

### What happens

The circuit breaker opens after consecutive failures (default: 3). Each instance falls back to local in-memory counters — identical behavior to running without Redis. Usage enforcement continues but with the per-instance blind spot restored. The `s3proxy_redis_fallback_active` gauge transitions to `1`.

A background health probe PINGs Redis every 5 seconds while the circuit is open. This requires no manual intervention.

### Recovery sequence

When the health probe detects Redis is reachable again:

1. Stale Redis keys for the current period are deleted (PG already absorbed those values via flushes during the outage)
2. Each instance INCRBYs its unflushed local deltas to Redis (additive — safe even if instances recover at different times)
3. Local counters are zeroed
4. Circuit breaker closes, shared operation resumes

### Monitoring

- `s3proxy_redis_fallback_active` — `1` when using local counters, `0` when Redis is healthy
- `s3proxy_redis_operations_total{operation,status}` — track Redis operation success/error rates
- `s3proxy_circuit_breaker_state{name="redis"}` — circuit breaker state (closed/open)

### Impact

Redis is a performance optimization, not a data dependency. All authoritative usage data lives in PostgreSQL. A Redis outage causes:

- **Temporary accuracy reduction** — same as running without Redis (per-instance counters with flush-gap)
- **No data loss** — PG flush continues via local counters, one instance per tick
- **Automatic recovery** — no operator action required

## Recovery Checklist

1. Identify the failure (database, backend, or orchestrator instance)
2. Restore the failed component
3. Check logs for circuit breaker state transitions
4. Run `s3-orchestrator admin status` to verify health
5. If database was restored from backup, run `sync` for each backend to reconcile
6. Monitor the cleanup queue for any stuck items
