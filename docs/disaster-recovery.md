---
description: "Failure scenarios and recovery procedures for a lost database, an unreachable backend, or an instance that will not come back up."
---

This guide covers failure scenarios and recovery procedures for the S3 Orchestrator.

## Architecture Context

The orchestrator has two required stateful components and one optional:

- **PostgreSQL** stores object locations, quota counters, usage stats, multipart state, and the cleanup queue. This is the source of truth for "which object lives on which backend." When encryption is enabled it also holds the only copy of each object's wrapped data key; see [Losing PostgreSQL without a backup](#losing-postgresql-without-a-backup).
- **Storage backends** (OCI, R2, S3, MinIO, etc.) hold the actual object data. These are independent and unaware of each other.
- **Redis** (optional) provides shared usage counters across instances. Not a data dependency - all authoritative data lives in PostgreSQL. See [Redis Failure](#redis-failure) below.

The orchestrator binary itself is stateless. Any instance with access to the database and backends can serve requests.

## PostgreSQL Failure

### What happens

When PostgreSQL becomes unreachable, the circuit breaker activates automatically:

- **Reads** enter degraded mode: the orchestrator consults a cache of recent key-to-backend lookups (`circuit_breaker.cache_ttl`, default 60s) and otherwise broadcasts GET requests to all backends. Set `circuit_breaker.degraded_reads_enabled: false` to fail reads fast instead. Each backend is listed to find the object's newest copy, since only the database records which `<key>!<id>` path a write used. Unencrypted objects found on any backend are returned normally, and compressed ones are decoded first. A deleted object whose backend delete failed and is waiting for retry can be served in that window. **Encrypted objects return `503 Service Unavailable`** because decryption requires encryption metadata (wrapped DEK, key ID) stored in the database.
- **Writes** return `503 Service Unavailable` because the orchestrator cannot record the object's location without the database.
- **Background workers** (rebalancer, replicator, usage flush, cleanup queue) pause until the database recovers.

The circuit breaker transitions back to healthy after successful database probes. No manual intervention is needed for recovery.

### Monitoring

Watch for these log messages, filtering on `breaker_name="database"`:

```
{"level":"WARN","msg":"opened: failure threshold reached","component":"circuit_breaker","breaker_name":"database","failures":3,"threshold":3}
{"level":"INFO","msg":"closed: recovered","component":"circuit_breaker","breaker_name":"database","degraded_duration":"42.1s"}
```

The `/health` endpoint continues to return `200 OK` with `"status":"degraded"` during database outages, so the service stays in load balancer rotation for reads.

### PostgreSQL High Availability

The orchestrator connects to PostgreSQL via a standard connection string and has no opinion about how the database is made highly available. Any PostgreSQL-compatible endpoint works, including:

- **Patroni** - open-source HA with automatic failover via etcd/ZooKeeper consensus
- **Amazon RDS Multi-AZ** / **Aurora** - managed failover with DNS endpoint
- **Google Cloud SQL HA** - regional instances with automatic failover
- **Neon / Supabase** - serverless PostgreSQL with built-in redundancy

When the database fails over to a replica, the circuit breaker briefly opens (writes return 503), then the probe detects the new primary and recovers automatically. The failover window is typically under 30 seconds with Patroni or managed services.

Writes are intentionally rejected during database outages rather than queued locally. This is a deliberate design choice: the database is the source of truth for object locations, and accepting writes without recording metadata would create orphaned objects that diverge from the metadata store. Use HA PostgreSQL to minimize the rejection window.

### Replication and the data loss window

Object replication is asynchronous by default. When a client writes an object, it is stored on a single backend and a 200 response is returned immediately. The background replication worker creates additional copies at the configured `replication.worker_interval` (default: 5 minutes).

If the primary backend fails before replication completes, unreplicated objects written in the last worker interval are at risk. To minimize this window:

- Turn on [`write_path.parallel_copies`](configuration.md#write_pathparallel_copies) so the write places its own copies instead of leaving them to the replicator. This shrinks the window from a worker interval to the time one upload takes, and it is the largest single reduction available
- Set `replication.worker_interval` to a lower value (e.g., `30s`) at the cost of more backend API calls
- Use backends with built-in durability guarantees (e.g., S3 Standard stores objects across 3+ availability zones)
- Monitor `s3o_replication_pending` - a sustained non-zero value indicates the replicator cannot keep up

**With `parallel_copies` on**, the window is not closed, only narrowed, and it is worth being precise about what remains. The client is answered on the first copy committed, so a second copy uploading at that moment is not yet durable: an instance lost in that window leaves the object on one backend, exactly as the default does, and the replicator repairs it. What changes is duration - the exposure lasts as long as one upload rather than as long as a replicator interval. Two counters say how often it happens: `s3o_replication_write_fanout_skipped_total` counts writes that placed a single copy because the in-flight ceiling was full, and `s3o_replication_write_copies_committed` is the distribution of copies per write, which sits at your replication factor when every write is placing them all.

A graceful shutdown waits up to 30 seconds for copies still uploading, so a planned restart does not itself create this exposure. A kill does; those copies are resolved by the pending reaper on a later tick.

Fully synchronous replication (holding the 200 until every copy lands) is not supported, and is deliberate: it would put the slowest backend on the critical path of every write.

## Losing PostgreSQL without a backup

Unencrypted objects survive the loss of the database. `sync` re-imports them from the backends, and they read normally afterwards.

Encrypted objects do not. Each one is encrypted with its own random data key, and that key, wrapped by the master key, is stored only in the object's database row. The master key cannot recreate a data key; it can only unwrap one. Without the database, `sync` and `reconcile` re-import the encrypted objects but record them as unmanaged with no key, so clients cannot list or read them. The objects are permanently unreadable, even with the master key and every backend intact.

When encryption is enabled, the database backup is therefore also the key backup. Treat it with the same care as the master key:

- Back it up on a schedule, and test restoring it.
- Keep backups long enough to recover from a loss that goes unnoticed for a while.
- Encrypt the backups at rest and restrict who can read them. A wrapped key is useless without the master key, so a stolen backup alone does not expose data, but a backup stored next to the master key does.

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

If backend circuit breakers are enabled and the backend refuses reads for longer than `replication.unhealthy_threshold` (default: 10 minutes), the replication worker automatically creates replacement copies on backends serving reads to maintain the configured replication factor. This restores full redundancy without manual intervention while the failed backend remains down.

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

### Backend refusing some calls

Some providers refuse one kind of call while still serving the others. The usual case is a monthly download allowance: once it runs out, every `GetObject` gets a `403` while uploads, deletes and listings keep working.

The per-call circuit breakers handle this without any action from you:

- **The GET breaker opens** after `failure_threshold` consecutive refusals. Only `GetObject` stops reaching the backend; deletes, listings and `HeadObject` carry on.
- **New objects go elsewhere.** A backend has to serve both `PutObject` and `GetObject` to be given new bytes, so client writes, replicas, rebalance moves and drain destinations all skip it. Without this, a new object's only copy could land where nobody can read it back.
- **Reads fail over** to another copy where one exists. An object whose only copy is on this backend can't be read until the refusal clears.
- **Replication fills the gap.** Once the backend has refused reads for longer than `replication.unhealthy_threshold` (default 10 minutes), its copies stop counting toward the factor. The replicator then copies those objects from their other copies to other backends. Objects with no other copy wait.
- **Recovery is automatic.** The GET breaker re-checks with a 1-byte read of the last key it refused. It waits `open_timeout` before the first check, then backs off up to 5 minutes between checks. When the provider serves downloads again, the breaker closes and the backend takes writes again.

How to see it:

- **`admin status`** shows the backend's health as `degraded` and lists `GetObject` under Refused.
- **The TUI and the web dashboard** show it as Degraded, with the refused calls named.
- **`s3o_backend_call_refused{backend="<name>",call="GetObject"}`** is `1`.
- **A `backend.circuit.opened` event** is published with `call: GetObject`.

What to do: usually nothing but wait for the provider's allowance to reset. A drain started now only drops the backend's copies of objects that have another copy; objects only it holds cannot be moved until downloads recover, since moving one means reading it. If you want the backend to stay out of placement after it recovers, [cordon](operations.md#cordoning-a-backend) it.

### Writes failing, reads working

If a backend refuses uploads but still serves reads, its PUT breaker opens and new copies go elsewhere on their own, while reads of what it holds carry on. The breaker closes and writes resume as soon as the backend accepts uploads again.

To keep it out of placement until you decide whether to wait out the provider or drain it, [cordon](operations.md#cordoning-a-backend) it. A cordon holds until you lift it, whatever the breaker does:

```bash
s3-orchestrator admin cordon <backend-name>
```

### Planned decommission

If a backend is still reachable but you want to decommission it, use the drain operation to migrate all objects to other backends first (no data loss):

```bash
# Start the drain - objects are migrated in the background
s3-orchestrator admin drain <backend-name>

# Monitor progress
s3-orchestrator admin drain-status <backend-name>

# Once complete, remove from config and restart
```

See the [Admin Guide](operations.md#draining-a-backend) for the full drain workflow.

### Preventing data loss

Enable replication with `replication.factor: 2` or higher so every object exists on at least two backends. This is the primary defense against backend loss.

## Cleanup Queue Recovery

Every transaction that removes a copy queues its bytes in the cleanup queue before it commits, and the instance that ran it deletes them right after. A delete that fails stays queued and is retried with exponential backoff (1 minute to 24 hours, max 10 attempts). Each row tracks `size_bytes`, and a backend's orphan bytes are the sum over its queued and dead-lettered rows, so the write path never overcommits storage on a backend with pending cleanups. A process that dies before settling its rows leaves them claimed; the worker reclaims them after `cleanup_queue.claim_grace_period` (default 5m). If items get stuck:

```bash
# Check the queue
s3-orchestrator admin cleanup-queue

# The background worker processes items automatically every minute.
# If the queue is backed up, check the logs for persistent errors.
```

Items that exhaust all 10 retry attempts are graduated to the `cleanup_dlq` table by the worker (single transaction: read the queue row, insert into `cleanup_dlq`, delete the queue row). The bytes are still on disk, so the DLQ row keeps them counted as orphan bytes, and the write path accounts for the unreleased space until an operator confirms the object is gone and writes off the row.

Monitor `s3o_cleanup_dlq_depth` (gauge) and `s3o_cleanup_dlq_enqueued_total{backend}` (counter): a non-zero depth means at least one unrecoverable orphan needs operator action; a single backend dominating the counter rate means that backend's delete path is broken and should be investigated.

List the dead-lettered rows, and push them back through automatic retry once the backend is fixed:

```bash
s3-orchestrator admin cleanup-dlq [-backend NAME]
s3-orchestrator admin cleanup-dlq requeue [-backend NAME]
```

Writing off a row has no command. After confirming the object is gone (via the reconciler, or by deleting it out-of-band), remove the row; removing it releases its bytes from the backend's orphan bytes:

```sql
DELETE FROM cleanup_dlq WHERE id = 42;
```

## Multi-Instance Recovery

When running multiple orchestrator instances:

- **Advisory locks** in PostgreSQL ensure only one instance runs each background worker (rebalancer, replicator, cleanup queue, lifecycle). If an instance crashes, the lock is released and another instance picks up the work.
- **No split-brain risk** for writes because PostgreSQL transactions serialize object location records. Two instances writing the same key will both succeed, but only one location record wins (the database is the arbiter).
- **Startup catch-up**: the replication worker runs an immediate reconciliation pass on startup before entering its periodic loop. This handles any objects that fell behind while instances were down.

## Redis Failure

When Redis is configured for shared usage counters:

### What happens

The circuit breaker opens after consecutive failures (default: 3). Each instance falls back to local in-memory counters - identical behavior to running without Redis. Usage enforcement continues but with the per-instance blind spot restored. The `s3o_redis_fallback_active` gauge transitions to `1`.

A background health probe PINGs Redis every 5 seconds while the circuit is open. This requires no manual intervention.

### Recovery sequence

When the health probe detects Redis is reachable again, the circuit breaker closes and shared operation resumes. Nothing is replayed into Redis: each instance flushes the charges it kept locally straight to the database on its next flush tick, under the month they were made in. Staged hashes a flush left in Redis before the outage are flushed by the next tick on any instance, and the flush id the database records with them keeps them from being counted twice.

### Monitoring

- `s3o_redis_fallback_active` - `1` when using local counters, `0` when Redis is healthy
- `s3o_redis_operations_total{operation,status}` - track Redis operation success/error rates
- `s3o_circuit_breaker_state{name="redis"}` - circuit breaker state (closed/open)

### Impact

Redis is a performance optimization, not a data dependency. All authoritative usage data lives in PostgreSQL. A Redis outage causes:

- **Temporary accuracy reduction** - same as running without Redis (per-instance counters with flush-gap)
- **No data loss** - PG flush continues via local counters, one instance per tick
- **Automatic recovery** - no operator action required

## Recovery Checklist

1. Identify the failure (database, backend, or orchestrator instance)
2. Restore the failed component
3. Check logs for circuit breaker state transitions
4. Run `s3-orchestrator admin status` to verify health
5. If database was restored from backup, run `sync` for each backend to reconcile
6. Monitor the cleanup queue for any stuck items
