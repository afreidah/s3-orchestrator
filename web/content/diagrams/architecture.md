---
description: "Interactive high-level diagram of the request path, storage layer, background services, and observability, with implementation notes."
title: "System Architecture"
linkTitle: "Architecture"
weight: -1
---

High-level architecture of the S3 Orchestrator showing the request path, storage layer, background services, and observability. **Hover over any component** for implementation details.

The gray background workers run only when the process starts in `worker` or `all` mode (`--mode`). The exceptions are the usage tracker's flush, the fleet snapshot and the circuit breaker watchdog, which run in every mode, plus the flight recorder and provisioning watch when enabled.

{{< diagram >}}

<script>
window.s3oDiagram = {
  source: [
    'flowchart TD',
    '    CLIENT([S3 Client]):::entry --> HTTP[HTTP Server<br>TLS / Timeouts]:::middleware',
    '    HTTP --> ROUTE{Path<br>Router}:::middleware',
    '    ROUTE -->|/admin/| ADMIN[Admin<br>API]:::handler',
    '    ROUTE -->|/ui/| WEBUI[Web<br>Dashboard]:::handler',
    '    ROUTE -->|S3 API| ADMIT[Admission Control<br>& Rate Limiter]:::middleware',
    '    ADMIT --> AUTH[SigV4 / Presigned<br>Authentication]:::middleware',
    '',
    '    AUTH -->|PUT/GET/HEAD/DELETE| OBJMGR[Object<br>Manager]:::handler',
    '    AUTH -->|multipart| MPMGR[Multipart<br>Manager]:::handler',
    '    AUTH -->|list/head bucket| BUCKETS[Bucket<br>Operations]:::handler',
    '',
    '    OBJMGR --> DCACHE[Object Data<br>Cache]:::storage',
    '    DCACHE -->|miss| COMP{Compression}:::storage',
    '    COMP --> ENC{Encryption}:::storage',
    '    MPMGR --> ENC',
    '    ENC -->|enabled| VAULT[Key Provider<br>Master key / Vault]:::data',
    '    ENC --> SELECT[Backend Selection<br>& Failover]:::storage',
    '',
    '    SELECT --> CB1[Circuit<br>Breaker]:::storage',
    '    CB1 --> BE1[S3 Backend 1]:::data',
    '    CB1 --> BE2[S3 Backend 2]:::data',
    '    CB1 --> BEN[S3 Backend N]:::data',
    '',
    '    OBJMGR --> DBCB[DB Circuit<br>Breaker]:::storage',
    '    MPMGR --> DBCB',
    '    BUCKETS --> DBCB',
    '    ADMIN --> OBJMGR',
    '    WEBUI --> OBJMGR',
    '    DBCB --> PG[(Metadata store<br>PostgreSQL or SQLite)]:::data',
    '    DBCB -->|open| BROADCAST[Broadcast<br>Reads]:::storage',
    '    BROADCAST --> CACHE[Location<br>Cache]:::storage',
    '    BROADCAST --> CB1',
    '',
    '    USAGE[Usage Tracker<br>& Quota Enforcement]:::background',
    '    USAGE --> COUNTER[Counter Backend<br>Local / Redis]:::data',
    '    USAGE --> PG',
    '    OBJMGR --> USAGE',
    '',
    '    WORKERS[Background<br>Workers]:::background',
    '    WORKERS --> SELECT',
    '    WORKERS --> CB1',
    '    WORKERS --> PG',
    '',
    '    subgraph OBS [Observability]',
    '        PROM[Prometheus<br>Metrics]:::observability',
    '        TEMPO[OpenTelemetry<br>Tracing]:::observability',
    '        AUDIT[Structured<br>Audit Logs]:::observability',
    '    end',
    '    HTTP --> OBS',
    '',
  ].join('\n'),

  info: {
    CLIENT: {
      title: 'S3 Client',
      badge: 'entry', badgeText: 'entry point',
      body: '<p>Any S3-compatible client: AWS CLI, rclone, SDKs, MinIO client, or custom applications.</p><p>Connects via standard S3 protocol with SigV4 authentication. Supports <code>Expect: 100-Continue</code> for large uploads.</p>'
    },
    HTTP: {
      title: 'HTTP Server',
      badge: 'middleware', badgeText: 'server',
      body: '<p>Go <code>net/http</code> server with configurable timeouts: <code>read_header_timeout</code>, <code>read_timeout</code>, <code>write_timeout</code>, <code>idle_timeout</code>.</p><p>Optional TLS with hot-reloadable certificates via <code>SIGHUP</code>. Supports mTLS with client CA verification.</p><p>Graceful shutdown: marks readiness false, waits for <code>shutdown_delay</code>, drains in-flight requests, flushes counters and traces.</p>'
    },
    ADMIT: {
      title: 'Admission Control & Rate Limiter',
      badge: 'middleware', badgeText: 'middleware',
      body: '<p>Both run before authentication, so a request is admitted or rejected before its signature is checked.</p><p><b>Admission Control:</b> Wraps only the S3 surface. Channel-based semaphore limiting concurrent in-flight requests. Global pool or separate read/write pools. Probabilistic load shedding ramps rejection from <code>server.load_shed_threshold</code> to capacity. Optional brief wait before hard rejection (<code>server.admission_wait</code>).</p><p><b>Rate Limiter:</b> Wraps the S3 surface and the admin API, not the dashboard. Per-IP token bucket using <code>golang.org/x/time/rate</code>. Extracts real client IP via X-Forwarded-For with trusted proxy CIDR validation.</p><p><a href="../admission-control/" style="color:#34b882">See detailed admission control flow diagram &rarr;</a></p>'
    },
    AUTH: {
      title: 'SigV4 / Presigned Authentication',
      badge: 'middleware', badgeText: 'authentication',
      body: '<p>Runs inside the S3 handler, after admission control and the rate limiter. The admin API and the dashboard authenticate inside their own handlers.</p><p>Verifies AWS Signature Version 4 from either the <code>Authorization</code> header or presigned URL query parameters. Reconstructs canonical request, derives signing key via HMAC-SHA256 chain, compares with <code>crypto/subtle.ConstantTimeCompare</code>.</p><p>The signing key is derived per request rather than cached so timing remains constant for known and unknown access keys alike. Presigned URLs validated via <code>X-Amz-Expires</code> (max 7 days).</p><p>Streaming-payload PUTs are validated end-to-end: the seed signature authenticates the request envelope, and a chunk-validating reader verifies each chained per-chunk signature (or the trailer signature for the unsigned-trailer variant) before any byte reaches storage.</p><p><code>BucketRegistry</code> resolves an access key to the user behind it, and that user answers whether it holds a grant on the bucket in the URL path. The registry is assembled from both sources a deployment declares credentials in - the config file and the store - and swapped whole behind an atomic pointer, so a credential issued or revoked through the provisioning API takes effect without a restart. The instance that handled the call rebuilds at once; other instances rebuild when the change arrives on the Redis provisioning channel, and without Redis only the handling instance rebuilds.</p>'
    },
    ROUTE: {
      title: 'Path Router',
      badge: 'middleware', badgeText: 'dispatcher',
      body: '<p>The server mux splits requests by path before any middleware runs:</p><p><b>/admin/:</b> admin API, behind the rate limiter<br><b>UI path:</b> dashboard (<code>ui.path</code>, default <code>/ui</code>)<br><b>/health, /health/ready:</b> health checks<br><b>/:</b> the S3 API, behind admission control and the rate limiter</p><p>After authentication the S3 handler dispatches by HTTP method, path, and query parameters:</p><p><b>Objects:</b> PUT, GET, HEAD, DELETE, CopyObject<br><b>Multipart:</b> CreateUpload, UploadPart, Complete, Abort, ListParts<br><b>Buckets:</b> ListObjects (v1/v2), HeadBucket, ListBuckets<br><b>Batch:</b> DeleteObjects (up to 1000 keys)</p>'
    },
    OBJMGR: {
      title: 'Object Manager',
      badge: 'handler', badgeText: 'core handler',
      body: '<p>CRUD operations with automatic failover. <b>PutObject</b>: filters backends by quota/health/draining, selects via routing strategy (spread/pack), retries on failure. <b>GetObject</b>: queries the metadata store for the object\'s copies, streams from a backend with failover to replicas. Supports Range requests and conditional headers.</p><p><b>DeleteObject</b> and <b>DeleteObjects</b>: one transaction removes the DB metadata and the key\'s in-flight intents and queues every copy\'s bytes in <code>cleanup_queue</code>; the copies are deleted from their backends after the commit, and a failed delete stays queued for retry.</p><p><b>CanAcceptWrite</b>: pre-flight check before body transmission (Expect: 100-Continue).</p><p><a href="../write-path/">Write path diagram &rarr;</a> &middot; <a href="../read-path/">Read path diagram &rarr;</a></p>'
    },
    MPMGR: {
      title: 'Multipart Manager',
      badge: 'handler', badgeText: 'handler',
      body: '<p>Manages multipart upload lifecycle. Each part upload is stored at a temporary path of its own (<code>__multipart/{uploadID}/{partNumber}!{id}</code>) until completion; a part recorded with no path of its own sits at <code>__multipart/{uploadID}/{partNumber}</code>.</p><p><b>Create</b>: selects backend, generates uploadID, records in DB.<br><b>UploadPart</b>: encrypts if enabled, claims the upload\'s backend with an intent (admitted against its room; a cordon taken after the upload started does not refuse it, a drain does), stores the part, records it in DB. Recording a re-uploaded part number queues the replaced part\'s bytes (reason <code>part_replaced</code>).<br><b>Complete</b>: marks the upload (<code>completion_heartbeat_at</code>) and renews the mark every 15 seconds while it assembles the final object; after the commit it clears the mark, and one transaction removes the upload and part rows and queues the parts\' bytes, deleted after that commit.<br><b>Abort</b>: the same transaction, removing the upload and part rows and queueing the parts\' bytes.<br><b>While a completion runs</b>: a part record, an abort, the stale-upload sweep and a second completion are refused with <code>409 OperationAborted</code>, so none can delete parts the assembly is reading. A mark not renewed for a minute belongs to a completion that died and no longer blocks the upload.</p><p><b>Bucket scope</b>: every per-uploadId request (<code>UploadPart</code>, <code>CompleteMultipartUpload</code>, <code>AbortMultipartUpload</code>, <code>ListParts</code>) is matched against the upload\'s stored <code>ObjectKey</code> prefix and rejected with <code>404 NoSuchUpload</code> when the URL implies a different bucket or key. The 404 is identical to the response for a non-existent upload so a caller cannot probe upload IDs across buckets by observing differing failure modes.</p>'
    },
    BUCKETS: {
      title: 'Bucket Operations',
      badge: 'handler', badgeText: 'handler',
      body: '<p><b>ListObjects</b> (v1 and v2): paginated listing from the metadata store with prefix filtering, delimiter support, and CommonPrefixes for directory simulation.</p><p><b>HeadBucket</b>: returns 200 if bucket exists (credential validation). <b>ListBuckets</b>: returns every bucket on which the caller\'s grants carry <code>list-buckets</code>. <b>GetBucketVersioning</b>: always returns disabled (not supported).</p>'
    },
    ADMIN: {
      title: 'Admin API',
      badge: 'handler', badgeText: 'handler',
      body: '<p>Operational control endpoints at <code>/admin/api/*</code>. Takes the same SigV4-signed credential the S3 surface does, and each route declares the permission a caller\'s grant has to carry.</p><p><b>Triggers:</b> usage-flush, usage-reconcile, replicate, rebalance, lifecycle, over-replication, scrub, reconcile, backfill-checksums, encrypt-existing, decrypt-existing, compress-existing, decompress-existing, rotate-encryption-key, cache flush.<br><b>Status:</b> status, reload-status, workers, cleanup-queue, cleanup-dlq, replication, cache, log-level.<br><b>Backends:</b> cordon and uncordon (stop new copies, keep serving reads); drain start, progress and cancel (moves all objects to other backends); backend removal.<br><b>Provisioning:</b> buckets, users, credentials and grants.</p><p>Health checks are <code>/health</code> and <code>/health/ready</code> on the main mux, not admin routes.</p>'
    },
    WEBUI: {
      title: 'Web Dashboard',
      badge: 'handler', badgeText: 'handler',
      body: '<p>Built-in web UI at <code>/ui/</code> with HMAC-signed session cookies (24h TTL) and login throttling (5 attempts per 5 min per IP).</p><p>Features: storage summary, per-backend quota bars, monthly usage charts, lazy-loaded directory tree, multi-file/folder upload, batch delete, rebalance trigger, real-time log viewer.</p><p>CSS cache busting via <code>?v={{.Version}}</code>.</p>'
    },
    DCACHE: {
      title: 'Object Data Cache',
      badge: 'storage', badgeText: 'optional',
      body: '<p>Optional in-memory LRU cache for object data. When enabled (<code>cache.enabled: true</code>), full GET responses are cached to avoid repeated backend fetches.</p><p>Entries are keyed by the storage path of the copy they came from. A GET looks up the key\'s location rows first and checks the cache for the paths they name.</p><p>On <b>cache hit</b>: returns the cached body &mdash; no backend API call, no egress, no decryption overhead.</p><p>On <b>cache miss</b>: proceeds through the normal path, then stores the response under the serving copy\'s path. Range requests always bypass the cache.</p><p>Every write stores its bytes under a new path, so no write invalidates anything and an overwritten object\'s entry is never reached again, on any instance.</p><p class="ac-metric">Config: cache.max_size, cache.max_object_size, cache.ttl</p>'
    },
    COMP: {
      title: 'Compression Layer',
      badge: 'storage', badgeText: 'optional',
      body: '<p>At-rest compression when <code>compression.enabled: true</code>, storing objects as chunked zstd in the Zstandard seekable format: one independently decodable frame per <code>chunk_size</code> of input, seek table in a trailing skippable frame.</p><p>Sits inside encryption, because ciphertext does not compress. Write order is compress then encrypt; read order is decrypt, decompress, slice.</p><p>The chunking is what keeps a partial read cheap. A single-frame object has one entry point, byte zero, so any range read would fetch the whole stored object and discard the prefix, at a cost proportional to object size rather than to the bytes asked for.</p><p>Objects below <code>min_size</code>, and objects that do not encode to at least <code>min_ratio</code> of their original size, are stored verbatim. A stored object is a valid Zstandard stream, so <code>zstd -d</code> decodes it without knowing about the seek table.</p><p><a href="../compression/">Compression flow diagram &rarr;</a> &middot; <a href="../../docs/compression/">Compression reference &rarr;</a></p>'
    },
    ENC: {
      title: 'Encryption Layer',
      badge: 'storage', badgeText: 'optional',
      body: '<p>Transparent envelope encryption when <code>encryption.enabled: true</code>.</p><p><b>Write:</b> generate random 256-bit DEK &rarr; wrap with master key &rarr; AES-256-GCM stream encrypt (64 KiB chunks) &rarr; store ciphertext + wrapped DEK in DB.</p><p><b>Read:</b> unwrap DEK &rarr; stream decrypt. <b>Range reads:</b> calculate affected chunks, fetch and decrypt only those.</p><p>ETag is MD5 of plaintext for S3 client compatibility.</p><p><a href="../encryption/">Encryption flow diagram &rarr;</a></p>'
    },
    VAULT: {
      title: 'Key Provider (Master key / Vault)',
      badge: 'data', badgeText: 'key source',
      body: '<p>Master key management for envelope encryption. Wraps/unwraps per-object Data Encryption Keys (DEKs).</p><p>Exactly one key source: <code>encryption.master_key</code> (inline base64), <code>encryption.master_key_file</code>, or <code>encryption.vault</code> (HashiCorp Vault Transit). For rotation, <code>encryption.previous_keys</code> and <code>encryption.previous_key_files</code> list old master keys that are used only to unwrap, so existing DEKs stay decryptable. Inline and file keys are recorded under their fingerprint, so a key keeps its ID across a rotation.</p>'
    },
    SELECT: {
      title: 'Backend Selection & Failover',
      badge: 'storage', badgeText: 'routing',
      body: '<p>Selects target backend using configured strategy:</p><p><b>spread:</b> picks least-utilized backend (equalizes storage across backends).<br><b>pack:</b> fills backends in order (consolidates storage, frees later backends).</p><p><b>Write failover:</b> on backend failure, removes from eligible list and retries next backend. <b>Read failover:</b> tries all replicas until one succeeds.</p><p>Writes start from <code>WriteTargets</code>: <code>Accepted</code> removes every backend carrying a state flag (cordoned, draining, drained, drain failed), and <code>Serving</code> removes every backend whose breakers refuse <code>PutObject</code> or <code>GetObject</code>. A client request is never sent to an open backend as a probe.</p>'
    },
    CB1: {
      title: 'Backend Circuit Breakers',
      badge: 'storage', badgeText: 'resilience',
      body: '<p>Optional, in two layers per backend, each <b>closed</b> (healthy) &rarr; <b>open</b> after N consecutive failures &rarr; <b>closed</b> once an out-of-band check passes. The backend breaker counts network errors and 401s and stops every call; each call\'s own breaker counts that call\'s 5xx, 429 and 403 and stops only that call.</p><p>A stopped call returns <code>ErrBackendUnavailable</code> immediately without I/O. Client requests are never used as probes. After <code>open_timeout</code>, the watchdog (5s tick) runs the breaker\'s check - <code>HeadBucket</code> for the backend breaker, a repeat of the call for a call breaker - backing off by doubling up to 5 minutes while it fails, and the breaker closes when it passes.</p><p>Config: <code>backend_circuit_breaker.enabled</code> (default off), <code>backend_circuit_breaker.failure_threshold</code> (default 5), <code>backend_circuit_breaker.open_timeout</code> (default 5m). The <code>circuit_breaker.*</code> keys configure the database breaker.</p><p class="ac-metric">Metrics: s3o_backend_call_refused{backend, call}, s3o_circuit_breaker_state, s3o_circuit_breaker_transitions_total &bull; Events: backend.circuit.opened, backend.circuit.closed, carrying <code>call</code></p><p><a href="../circuit-breaker/">Circuit breaker state machine diagram &rarr;</a></p>'
    },
    BE1: {
      title: 'S3 Backend',
      badge: 'data', badgeText: 'storage',
      body: '<p>AWS SDK v2 S3 client wrapping any S3-compatible endpoint: AWS S3, OCI Object Storage, Cloudflare R2, MinIO, Backblaze B2, GCS (with header stripping).</p><p>HTTP transport: 100 idle / 200 max connections per host, 30s keep-alive, 60s idle timeout (forces DNS refresh), HTTP/2 support.</p><p>Each backend has configurable quota, API request limits, and egress/ingress byte limits (monthly rolling).</p>'
    },
    BE2: {
      title: 'S3 Backend',
      badge: 'data', badgeText: 'storage',
      body: '<p>Additional S3-compatible backend. Multiple backends enable multi-cloud redundancy, free-tier aggregation, and automatic failover.</p><p>Backends can be different providers (e.g., OCI + R2 + MinIO) with independent quotas and limits.</p>'
    },
    BEN: {
      title: 'S3 Backend',
      badge: 'data', badgeText: 'storage',
      body: '<p>N backends supported. Each independently managed with its own circuit breaker, quota tracking, and usage limits.</p><p>Backends can be added/removed via config with hot reload (SIGHUP). Decommission with the drain API to safely move all objects off a backend before removal.</p>'
    },
    CACHE: {
      title: 'Location Cache',
      badge: 'storage', badgeText: 'caching',
      body: '<p>In-memory cache mapping object keys to the backend that last served them during a degraded read. Used only by broadcast reads while the database breaker is open; normal reads always query the metadata store.</p><p>A degraded read tries the cached backend first and falls back to the broadcast if that fails. The winner of each broadcast is cached. Entries expire after <code>circuit_breaker.cache_ttl</code> (default 60s) and are removed when the object is written or deleted.</p>'
    },
    DBCB: {
      title: 'DB Circuit Breaker',
      badge: 'storage', badgeText: 'resilience',
      body: '<p>Circuit breaker wrapping the metadata store. Detects DB outages and returns <code>ErrDBUnavailable</code> sentinel.</p><p>When open, reads fall back to <b>broadcast reads</b>: the backends are tried in turn (in parallel when <code>circuit_breaker.parallel_broadcast</code> is on) and the first successful response is returned. Writes return 503. <code>circuit_breaker.degraded_reads_enabled: false</code> turns the fallback off so reads fail fast too.</p><p>Only actual DB errors trip the breaker; application-level errors (ErrNoSpaceAvailable, S3Error) are excluded.</p><p><a href="../circuit-breaker/">Circuit breaker state machine diagram &rarr;</a></p>'
    },
    PG: {
      title: 'Metadata store (PostgreSQL or SQLite)',
      badge: 'data', badgeText: 'metadata store',
      body: '<p><code>database.driver</code> selects <code>sqlite</code> (the default, single instance) or <code>postgres</code> (required for multiple instances). When the driver is unset, setting <code>database.host</code> selects PostgreSQL.</p><p>Stores all object metadata, locations, multipart state, quotas, usage counters, cleanup queue, and replication state.</p><p>Tables: <code>object_locations</code>, <code>object_tags</code>, <code>multipart_uploads</code>, <code>multipart_parts</code>, <code>backend_quotas</code>, <code>backend_quota_stripes</code>, <code>backend_usage</code>, <code>backend_request_usage</code>, <code>backend_states</code>, <code>cleanup_queue</code>, <code>cleanup_dlq</code>, <code>pending_objects</code>, <code>notification_outbox</code>, <code>buckets</code>, <code>users</code>, <code>credentials</code>, <code>grants</code>.</p><p>Write admission reads the <code>backend_capacity</code> view, which derives each backend\'s available bytes from its quota, used and orphan bytes, in-flight multipart parts and pending intents, and carries the backend\'s state flags, which admission tests against the write accept mask, so a cordoned or drain-flagged backend takes no new writes.</p><p>Background workers coordinate through advisory locks. On PostgreSQL the connection pool is pgx with configurable <code>max_conns</code>, <code>min_conns</code>, <code>max_conn_lifetime</code>. Migrations are applied automatically on startup.</p>'
    },
    BROADCAST: {
      title: 'Broadcast Reads',
      badge: 'storage', badgeText: 'fallback',
      body: '<p>Degraded-mode read path used when the DB circuit breaker is open. It first tries the backend the location cache holds for the key, then the backends in configured order.</p><p>By default the backends are tried one at a time. With <code>circuit_breaker.parallel_broadcast: true</code> they are probed at once, capped by <code>circuit_breaker.degraded_broadcast_parallelism</code> (0 is uncapped), and the remaining probes are cancelled when one succeeds.</p><p>With no row to say which <code>&lt;key&gt;!&lt;id&gt;</code> path holds the bytes, each backend tried is listed from the object key and the most recently modified of the bare key and its copies is read. Its bytes are then checked the way import checks them: a compressed copy is decoded, an encrypted one returns 503.</p><p>Reads continue during a metadata store outage as long as at least one backend holds the object. <code>circuit_breaker.degraded_reads_enabled: false</code> turns this path off.</p><p><a href="../read-path/">Read path diagram &rarr;</a></p>'
    },
    USAGE: {
      title: 'Usage Tracker & Quota Enforcement',
      badge: 'background', badgeText: 'quota',
      body: '<p>Tracks per-backend monthly counters: API requests, egress bytes, ingress bytes, and one count per configured request pool. Effective usage = DB baseline + unflushed in-memory deltas.</p><p>Requests are budgeted per pool because providers meter operation classes separately: an upload and a read draw on different allowances, and some operations are not billed at all. Every call is still counted against the request total, whether or not a budget charges it.</p><p>Writes pick from <code>EligibleForWrite</code>, which starts from <code>WriteTargets</code> (no state flag set, serving <code>PutObject</code> and <code>GetObject</code>) and then applies the usage filter (<code>UsageTracker.WithinLimits</code> plus the per-backend max object size). Flushes deltas to DB every <code>usage_flush.interval</code> (default 30s). With <code>usage_flush.adaptive_enabled: true</code>, the interval drops to <code>usage_flush.fast_interval</code> (default 5s) when a backend nears a limit.</p><p>With Redis: shared counters across instances, advisory lock prevents destructive concurrent flushes.</p>'
    },
    COUNTER: {
      title: 'Counter Backend',
      badge: 'data', badgeText: 'counters',
      body: '<p><b>Local:</b> atomic in-process counters (single instance).<br><b>Redis:</b> shared counters for multi-instance deployments.</p><p>Redis fallback: if Redis is down, counters accumulate locally and flush to DB on the next successful cycle. No data loss, just temporary per-instance isolation.</p>'
    },
    WORKERS: {
      title: 'Background Workers',
      badge: 'background', badgeText: 'background workers',
      body: '<p>The replicator, rebalancer, drainer, over-replication cleaner, cleanup queue, pending reaper, scrubber, reconciler, lifecycle expiration, multipart cleanup and fleet snapshot. Each runs on its own interval under an advisory lock, so only one instance works a given task at a time.</p><p>They read and write the metadata store, and move or delete bytes through backend selection and the per-backend circuit breakers. Most run only in <code>worker</code> or <code>all</code> mode.</p><p><a href="../background-services/">Background services diagram &rarr;</a></p>'
    },
    PROM: {
      title: 'Prometheus Metrics',
      badge: 'observability', badgeText: 'observability',
      body: '<p>Exposed at <code>/metrics</code>. Counters: requests, failovers, rejections, replication ops, cleanup ops, encryption ops. Gauges: quota (used/limit/free per backend), object counts, queue depth, build info. The fleet-wide gauges are set by the fleet snapshot every <code>fleet_interval</code>. Histograms: request duration, request/response sizes, backend latency.</p><p>Per-backend metrics with <code>backend</code> label. Circuit breaker state as gauge (0=closed, 1=open, 2=half-open).</p>'
    },
    TEMPO: {
      title: 'OpenTelemetry Tracing',
      badge: 'observability', badgeText: 'observability',
      body: '<p>Distributed tracing exported to Tempo (or any OTLP-compatible collector). Configurable sample rate (0&ndash;1.0).</p><p>Spans: HTTP request (root) &rarr; auth &rarr; manager operation &rarr; backend I/O. Attributes include operation name, bucket, key, request ID, backend name, status code, object size.</p><p>Request ID (<code>s3o.request_id</code>) links traces to audit logs.</p>'
    },
    AUDIT: {
      title: 'Structured Audit Logs',
      badge: 'observability', badgeText: 'observability',
      body: '<p>Structured <code>slog</code> entries with <code>audit=true</code> marker. Two-level audit: HTTP envelope (method, path, status, duration) + storage operation (backend, key, outcome).</p><p>Request IDs flow through context from HTTP layer to storage layer. Background services get auto-generated correlation IDs per tick.</p><p>JSON output to stdout for log collectors. Recent entries buffered in-memory for the Web UI log viewer.</p><p class="ac-metric">Metric: s3o_audit_events_total</p>'
    }
  }
};
</script>

## Legend

| Node | Meaning |
|------|---------|
| <span class="dg-swatch dg-entry"></span> | Entry point |
| <span class="dg-swatch dg-filter"></span> | Middleware / routing |
| <span class="dg-swatch dg-process"></span> | Request handlers |
| <span class="dg-swatch dg-storage"></span> | Internal storage layer |
| <span class="dg-swatch dg-success"></span> | External data stores |
| <span class="dg-swatch dg-background"></span> | Background services |
| <span class="dg-swatch dg-reject"></span> | Observability |
