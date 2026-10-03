---
description: "Interactive high-level diagram of the request path, storage layer, background services, and observability, with implementation notes."
title: "System Architecture"
linkTitle: "Architecture"
weight: -1
---

High-level architecture of the S3 Orchestrator showing the request path, storage layer, background services, and observability. **Hover over any component** for implementation details.

The gray background workers run only when the process starts in `worker` or `all` mode (`--mode`). The exceptions are the usage tracker's flush and the backend circuit breaker watchdog, which run in every mode.

<style>
  #ac-diagram { margin: 1rem 0; }

  /* floating tooltip */
  #ac-tooltip {
    position: fixed; z-index: 9999;
    max-width: 380px; padding: 0.7rem 0.85rem;
    background: #161b22; border: 1px solid #30363d; border-radius: 6px;
    box-shadow: 0 4px 16px rgba(0,0,0,0.4);
    display: none;
  }
  #ac-tooltip a { color: #34b882; text-decoration: none; }
  #ac-tooltip a:hover { text-decoration: underline; }
  #ac-tooltip h3 { color: #2a9d73; font-size: 0.85rem; margin: 0 0 0.25rem 0; }
  #ac-tooltip .ac-badge {
    display: inline-block; padding: 1px 7px; border-radius: 4px;
    font-size: 0.6rem; font-weight: 600; margin-bottom: 0.4rem; text-transform: uppercase;
  }
  .ac-badge-entry { background: #1a7a5a22; color: #34b882; border: 1px solid #34b88255; }
  .ac-badge-middleware { background: #9e6a0322; color: #d29922; border: 1px solid #d2992255; }
  .ac-badge-handler { background: #8957e522; color: #bc8cff; border: 1px solid #bc8cff55; }
  .ac-badge-storage { background: #1a3a3022; color: #4aaa8a; border: 1px solid #4aaa8a55; }
  .ac-badge-data { background: #23863622; color: #3fb950; border: 1px solid #3fb95055; }
  .ac-badge-background { background: #8b949e22; color: #8b949e; border: 1px solid #8b949e55; }
  .ac-badge-observability { background: #da363322; color: #f85149; border: 1px solid #f8514955; }
  #ac-tooltip p { font-size: 0.75rem; line-height: 1.4; color: #c9d1d9; margin-bottom: 0.35rem; }
  #ac-tooltip code { background: #21262d; padding: 1px 4px; border-radius: 3px; font-size: 0.7rem; color: #4aaa8a; }
  #ac-tooltip .ac-metric { color: #a7d5c1; font-style: italic; font-size: 0.7rem; }

  /* path highlighting */
  #ac-diagram .node, #ac-diagram .edgePath, #ac-diagram .edgeLabel { transition: opacity 0.15s, filter 0.15s; }
  #ac-diagram svg.highlighting .node, #ac-diagram svg.highlighting .edgePath, #ac-diagram svg.highlighting .edgeLabel { opacity: 0.12; }
  #ac-diagram svg.highlighting .node.highlight, #ac-diagram svg.highlighting .edgePath.highlight, #ac-diagram svg.highlighting .edgeLabel.highlight { opacity: 1; filter: drop-shadow(0 0 6px rgba(42,157,115,0.5)); }
  #ac-diagram .node { cursor: pointer; }
</style>

<div id="ac-diagram"></div>
<div id="ac-tooltip"></div>

<script src="https://cdn.jsdelivr.net/npm/mermaid@11.8.0/dist/mermaid.min.js"></script>
<script>
(function() {
  var diagramSrc = [
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
    '    REPL[Replicator]:::background --> SELECT',
    '    REPL --> PG',
    '    REBAL[Rebalancer]:::background --> SELECT',
    '    REBAL --> PG',
    '    CLEAN[Cleanup<br>Queue]:::background --> CB1',
    '    CLEAN --> PG',
    '    LIFE[Lifecycle<br>Expiration]:::background --> OBJMGR',
    '    MPCLEAN[Multipart<br>Cleanup]:::background --> MPMGR',
    '    OVERREP[Over-Replication<br>Cleaner]:::background --> SELECT',
    '    OVERREP --> PG',
    '    PENDREAP[Pending<br>Reaper]:::background --> PG',
    '    PENDREAP --> CB1',
    '    DRAIN[Drainer]:::background --> SELECT',
    '    DRAIN --> PG',
    '    SCRUB[Scrubber]:::background --> CB1',
    '    SCRUB --> PG',
    '    RECON[Reconciler]:::background --> CB1',
    '    RECON --> PG',
    '',
    '    HTTP --> PROM[Prometheus<br>Metrics]:::observability',
    '    HTTP --> TEMPO[OpenTelemetry<br>Tracing]:::observability',
    '    HTTP --> AUDIT[Structured<br>Audit Logs]:::observability',
    '',
    '    classDef entry fill:#1a7a5a,stroke:#1a7a5a,color:#fff,font-weight:bold',
    '    classDef middleware fill:#6b5b2e,stroke:#c4a35a,color:#fff',
    '    classDef handler fill:#2d7d6a,stroke:#5ec9a0,color:#fff',
    '    classDef storage fill:#1a3a30,stroke:#4aaa8a,color:#c9d1d9',
    '    classDef data fill:#1a7a5a,stroke:#34b882,color:#fff',
    '    classDef background fill:#222a26,stroke:#8a9aa8,color:#e6edf3',
    '    classDef observability fill:#8b3a3a,stroke:#d4a0a0,color:#fff'
  ].join('\n');

  mermaid.initialize({
    startOnLoad: false,
    theme: 'base',
    themeVariables: {
      darkMode: true,
      background: '#191c23',
      fontFamily: 'Inter, ui-sans-serif, system-ui, sans-serif',
      fontSize: '15px',
      primaryColor: '#26332f',
      primaryTextColor: '#f8fafc',
      primaryBorderColor: '#2a9d73',
      secondaryColor: '#3a2e20',
      secondaryTextColor: '#e8dfd0',
      secondaryBorderColor: '#c4a35a',
      tertiaryColor: '#20262d',
      tertiaryTextColor: '#e8dfd0',
      tertiaryBorderColor: '#4aaa8a',
      lineColor: '#7f8b86',
      edgeLabelBackground: '#191c23',
      clusterBkg: '#1d2229',
      clusterBorder: '#39443f'
    },
    flowchart: { nodeSpacing: 32, rankSpacing: 46, curve: 'linear', padding: 12, diagramPadding: 16, useMaxWidth: true, htmlLabels: true }
  });

  mermaid.render('arch-mermaid-svg', diagramSrc).then(function(result) {
    document.getElementById('ac-diagram').innerHTML = result.svg;
    wireUpInteractivity();
  });

  var nodeInfo = {
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
      body: '<p>CRUD operations with automatic failover. <b>PutObject</b>: filters backends by quota/health/draining, selects via routing strategy (spread/pack), retries on failure. <b>GetObject</b>: queries the metadata store for the object\'s copies, streams from a backend with failover to replicas. Supports Range requests and conditional headers.</p><p><b>DeleteObject</b>: removes from all backends + DB metadata. Failed deletions enqueued to <code>cleanup_queue</code>.</p><p><b>CanAcceptWrite</b>: pre-flight check before body transmission (Expect: 100-Continue).</p><p><a href="../write-path/">Write path diagram &rarr;</a> &middot; <a href="../read-path/">Read path diagram &rarr;</a></p>'
    },
    MPMGR: {
      title: 'Multipart Manager',
      badge: 'handler', badgeText: 'handler',
      body: '<p>Manages multipart upload lifecycle. Parts stored at temporary keys (<code>__multipart/{uploadID}/{partNumber}</code>) until completion.</p><p><b>Create</b>: selects backend, generates uploadID, records in DB.<br><b>UploadPart</b>: encrypts if enabled, stores part, records in DB.<br><b>Complete</b>: assembles final object, cleans up temporary parts.<br><b>Abort</b>: deletes temporary parts + DB records.</p><p><b>Bucket scope</b>: every per-uploadId request (<code>UploadPart</code>, <code>CompleteMultipartUpload</code>, <code>AbortMultipartUpload</code>, <code>ListParts</code>) is matched against the upload\'s stored <code>ObjectKey</code> prefix and rejected with <code>404 NoSuchUpload</code> when the URL implies a different bucket or key. The 404 is identical to the response for a non-existent upload so a caller cannot probe upload IDs across buckets by observing differing failure modes.</p>'
    },
    BUCKETS: {
      title: 'Bucket Operations',
      badge: 'handler', badgeText: 'handler',
      body: '<p><b>ListObjects</b> (v1 and v2): paginated listing from the metadata store with prefix filtering, delimiter support, and CommonPrefixes for directory simulation.</p><p><b>HeadBucket</b>: returns 200 if bucket exists (credential validation). <b>ListBuckets</b>: returns every bucket on which the caller\'s grants carry <code>list-buckets</code>. <b>GetBucketVersioning</b>: always returns disabled (not supported).</p>'
    },
    ADMIN: {
      title: 'Admin API',
      badge: 'handler', badgeText: 'handler',
      body: '<p>Operational control endpoints at <code>/admin/api/*</code>. Takes the same SigV4-signed credential the S3 surface does, and each route declares the permission a caller\'s grant has to carry.</p><p><b>Triggers:</b> usage-flush, usage-reconcile, replicate, rebalance, lifecycle, over-replication, scrub, reconcile, backfill-checksums, encrypt-existing, decrypt-existing, compress-existing, decompress-existing, cache flush.<br><b>Status:</b> status, reload-status, workers, cleanup-queue, cleanup-dlq, replication, cache, log-level.<br><b>Backends:</b> drain start, progress and cancel (moves all objects to other backends); backend removal.<br><b>Provisioning:</b> buckets, users, credentials and grants.</p><p>Health checks are <code>/health</code> and <code>/health/ready</code> on the main mux, not admin routes.</p>'
    },
    WEBUI: {
      title: 'Web Dashboard',
      badge: 'handler', badgeText: 'handler',
      body: '<p>Built-in web UI at <code>/ui/</code> with HMAC-signed session cookies (24h TTL) and login throttling (5 attempts per 5 min per IP).</p><p>Features: storage summary, per-backend quota bars, monthly usage charts, lazy-loaded directory tree, multi-file/folder upload, batch delete, rebalance trigger, real-time log viewer.</p><p>CSS cache busting via <code>?v={{.Version}}</code>.</p>'
    },
    DCACHE: {
      title: 'Object Data Cache',
      badge: 'storage', badgeText: 'optional',
      body: '<p>Optional in-memory LRU cache for object data. When enabled (<code>cache.enabled: true</code>), full GET responses are cached to avoid repeated backend fetches.</p><p>On <b>cache hit</b>: returns the cached body immediately &mdash; no backend API call, no egress, no decryption overhead.</p><p>On <b>cache miss</b>: proceeds through the normal path, then stores the response for future reads. Range requests always bypass the cache.</p><p>Automatically invalidated on PutObject, DeleteObject, CopyObject, and CompleteMultipartUpload.</p><p class="ac-metric">Config: cache.max_size, cache.max_object_size, cache.ttl</p>'
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
      body: '<p>Master key management for envelope encryption. Wraps/unwraps per-object Data Encryption Keys (DEKs).</p><p>Exactly one key source: <code>encryption.master_key</code> (inline base64), <code>encryption.master_key_file</code>, or <code>encryption.vault</code> (HashiCorp Vault Transit). For rotation, <code>encryption.previous_keys</code> lists old master keys that are used only to unwrap, so existing DEKs stay decryptable.</p>'
    },
    SELECT: {
      title: 'Backend Selection & Failover',
      badge: 'storage', badgeText: 'routing',
      body: '<p>Selects target backend using configured strategy:</p><p><b>spread:</b> picks least-utilized backend (equalizes storage across backends).<br><b>pack:</b> fills backends in order (consolidates storage, frees later backends).</p><p><b>Write failover:</b> on backend failure, removes from eligible list and retries next backend. <b>Read failover:</b> tries all replicas until one succeeds.</p><p><code>ExcludeUnhealthy</code> removes every backend whose circuit breaker is open, and <code>ExcludeDraining</code> removes draining ones. A client request is never sent to an open backend as a probe.</p>'
    },
    CB1: {
      title: 'Backend Circuit Breakers',
      badge: 'storage', badgeText: 'resilience',
      body: '<p>Optional per-backend breaker: <b>closed</b> (healthy) &rarr; <b>open</b> after N consecutive failures &rarr; <b>closed</b> once an out-of-band health check passes. Counted failures are network errors, 5xx, 429, and credential rejections (401/403).</p><p>When open, every call returns <code>ErrBackendUnavailable</code> immediately without I/O. Client requests are never used as probes. After <code>open_timeout</code>, the watchdog (5s tick) sends a <code>HeadBucket</code> health check, and the breaker closes when it passes.</p><p>Config: <code>backend_circuit_breaker.enabled</code> (default off), <code>backend_circuit_breaker.failure_threshold</code> (default 5), <code>backend_circuit_breaker.open_timeout</code> (default 5m). The <code>circuit_breaker.*</code> keys configure the database breaker.</p><p class="ac-metric">Metrics: s3o_circuit_breaker_state, s3o_circuit_breaker_transitions_total</p><p><a href="../circuit-breaker/">Circuit breaker state machine diagram &rarr;</a></p>'
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
      body: '<p><code>database.driver</code> selects <code>sqlite</code> (the default, single instance) or <code>postgres</code> (required for multiple instances). When the driver is unset, setting <code>database.host</code> selects PostgreSQL.</p><p>Stores all object metadata, locations, multipart state, quotas, usage counters, cleanup queue, and replication state.</p><p>Tables: <code>object_locations</code>, <code>object_tags</code>, <code>multipart_uploads</code>, <code>multipart_parts</code>, <code>backend_quotas</code>, <code>backend_quota_stripes</code>, <code>backend_usage</code>, <code>backend_request_usage</code>, <code>backend_drains</code>, <code>cleanup_queue</code>, <code>cleanup_dlq</code>, <code>pending_objects</code>, <code>notification_outbox</code>, <code>buckets</code>, <code>users</code>, <code>credentials</code>, <code>grants</code>.</p><p>Write admission reads the <code>backend_capacity</code> view, which derives each backend\'s available bytes from its quota, used and orphan bytes, in-flight multipart parts and pending intents, and marks a draining backend as not accepting writes.</p><p>Background workers coordinate through advisory locks. On PostgreSQL the connection pool is pgx with configurable <code>max_conns</code>, <code>min_conns</code>, <code>max_conn_lifetime</code>. Migrations are applied automatically on startup.</p>'
    },
    BROADCAST: {
      title: 'Broadcast Reads',
      badge: 'storage', badgeText: 'fallback',
      body: '<p>Degraded-mode read path used when the DB circuit breaker is open. It first tries the backend the location cache holds for the key, then the backends in configured order.</p><p>By default the backends are tried one at a time. With <code>circuit_breaker.parallel_broadcast: true</code> they are probed at once, capped by <code>circuit_breaker.degraded_broadcast_parallelism</code> (0 is uncapped), and the remaining probes are cancelled when one succeeds.</p><p>With no row to say which <code>&lt;key&gt;!&lt;id&gt;</code> path holds the bytes, each backend tried is listed from the object key and the most recently modified of the bare key and its copies is read. Its bytes are then checked the way import checks them: a compressed copy is decoded, an encrypted one returns 503.</p><p>Reads continue during a metadata store outage as long as at least one backend holds the object. <code>circuit_breaker.degraded_reads_enabled: false</code> turns this path off.</p><p><a href="../read-path/">Read path diagram &rarr;</a></p>'
    },
    USAGE: {
      title: 'Usage Tracker & Quota Enforcement',
      badge: 'background', badgeText: 'quota',
      body: '<p>Tracks per-backend monthly counters: API requests, egress bytes, ingress bytes, and one count per configured request pool. Effective usage = DB baseline + unflushed in-memory deltas.</p><p>Requests are budgeted per pool because providers meter operation classes separately: an upload and a read draw on different allowances, and some operations are not billed at all. Every call is still counted against the request total, whether or not a budget charges it.</p><p>Writes pick from <code>EligibleForWrite</code>, which drops draining and circuit-broken backends and then applies the usage filter (<code>UsageTracker.WithinLimits</code> plus the per-backend max object size). Flushes deltas to DB every <code>usage_flush.interval</code> (default 30s). With <code>usage_flush.adaptive_enabled: true</code>, the interval drops to <code>usage_flush.fast_interval</code> (default 5s) when a backend nears a limit.</p><p>With Redis: shared counters across instances, advisory lock prevents destructive concurrent flushes.</p>'
    },
    COUNTER: {
      title: 'Counter Backend',
      badge: 'data', badgeText: 'counters',
      body: '<p><b>Local:</b> atomic in-process counters (single instance).<br><b>Redis:</b> shared counters for multi-instance deployments.</p><p>Redis fallback: if Redis is down, counters accumulate locally and flush to DB on the next successful cycle. No data loss, just temporary per-instance isolation.</p>'
    },
    REPL: {
      title: 'Replicator',
      badge: 'background', badgeText: 'background worker',
      body: '<p>Creates additional copies of under-replicated objects. Runs every 5 minutes under advisory lock (<code>LockReplicator</code>).</p><p>Queries DB for objects with fewer copies than <code>replication.factor</code>. Stream-copies to backends with available quota (binary copy, no re-encryption).</p><p>A copy on a backend whose breaker has been open longer than <code>replication.unhealthy_threshold</code> (default 10m) stops counting toward the factor, so a replacement is made elsewhere.</p><p><a href="../background-services/">Background services coordination diagram &rarr;</a></p>'
    },
    REBAL: {
      title: 'Rebalancer',
      badge: 'background', badgeText: 'background worker',
      body: '<p>Redistributes objects across backends to optimize storage utilization. Disabled by default (<code>rebalance.enabled</code>). Runs every <code>rebalance.interval</code> (default 6 hours) under advisory lock.</p><p><b>spread:</b> equalizes utilization ratios across backends.<br><b>pack:</b> consolidates objects into fewer backends, freeing up others.</p><p>Moves objects: stream copy from source to target (binary copy, no re-encryption) &rarr; swap the DB location with compare-and-swap &rarr; delete the source copy.</p><p><a href="../background-services/">Background services coordination diagram &rarr;</a></p>'
    },
    CLEAN: {
      title: 'Cleanup Queue',
      badge: 'background', badgeText: 'background worker',
      body: '<p>Retries failed backend deletions with exponential backoff (1 minute to 24 hours, max 10 attempts). Runs every minute, processes up to 50 items with 10 concurrent goroutines.</p><p>Each tick uses <code>ClaimPendingCleanups</code> (<code>UPDATE ... WHERE id IN (SELECT ... FOR UPDATE SKIP LOCKED)</code>) to atomically reserve rows and stamp the calling instance\'s identifier so concurrent ticks across instances always see disjoint sets. A claim older than <code>cleanup_queue.claim_grace_period</code> (default 5m) is reclaimable so a worker that died mid-process does not leave a row stuck. Successful retries call <code>CompleteCleanupItem</code>, which deletes the row and decrements <code>orphan_bytes</code> in a single atomic CTE.</p><p>Enqueued at all failure sites: PutObject rollback, DeleteObject, multipart abort/complete, rebalancer, replicator.</p><p>On the tenth consecutive failure the row is graduated to <code>cleanup_dlq</code> via <code>core.MoveCleanupToDLQ</code> and the <code>cleanup.exhausted</code> notification is emitted; <code>orphan_bytes</code> is intentionally untouched because the bytes are still on disk.</p><p class="ac-metric">Metrics: s3o_cleanup_queue_depth, s3o_cleanup_queue_processed_total, s3o_cleanup_queue_stale_claims_recovered_total{backend}, s3o_cleanup_dlq_depth, s3o_cleanup_dlq_enqueued_total{backend}</p><p><a href="../background-services/">Background services coordination diagram &rarr;</a></p>'
    },
    LIFE: {
      title: 'Lifecycle Expiration',
      badge: 'background', badgeText: 'background worker',
      body: '<p>Automatic object expiration. Each rule filters on <code>prefix</code>, <code>tags</code>, or both, and sets <code>expiration_days</code>. Runs hourly under advisory lock.</p><p>Example: <code>prefix: temp/, expiration_days: 30</code> deletes all objects under <code>temp/</code> older than 30 days. Calls ObjectManager.DeleteObject for proper cleanup across all backends.</p><p><a href="../background-services/">Background services coordination diagram &rarr;</a></p>'
    },
    MPCLEAN: {
      title: 'Multipart Cleanup',
      badge: 'background', badgeText: 'background worker',
      body: '<p>Finds incomplete multipart uploads older than 24 hours and aborts them. Runs hourly under advisory lock.</p><p>Prevents orphaned temporary parts (<code>__multipart/{uploadID}/*</code>) from accumulating on backends when clients abandon uploads.</p><p><a href="../background-services/">Background services coordination diagram &rarr;</a></p>'
    },
    OVERREP: {
      title: 'Over-Replication Cleaner',
      badge: 'background', badgeText: 'background worker',
      body: '<p>Finds objects with more copies than the configured replication factor and deletes excess replicas. Runs every <code>replication.worker_interval</code> (default 5 minutes) under advisory lock <code>LockOverReplication</code> (1008).</p><p>Copies are scored by backend state: copies on draining backends go first, then copies on circuit-broken backends, then copies on the most utilized healthy backends. Frees up quota where it is scarcest.</p><p><a href="../background-services/">Background services coordination diagram &rarr;</a></p>'
    },
    PENDREAP: {
      title: 'Pending Reaper',
      badge: 'background', badgeText: 'background worker',
      body: '<p>Resolves unresolved <code>pending_objects</code> rows from the write-path PUT-before-COMMIT pattern. If the orchestrator died between the backend PUT and the metadata commit, this worker is what makes the orphan recoverable: it HEADs the backend and either promotes the intent (HEAD 200) or drops it (HEAD 404).</p><p>Runs every <code>write_path.pending_pattern.reaper_tick</code> (default 1 min), processing intents older than <code>min_age</code> (default 5 min) in batches of <code>batch_size</code> (default 50) with concurrency 4. Each tick runs under advisory lock <code>LockPendingReaper</code> (1011), and each intent is re-read under <code>FOR UPDATE</code> before it is resolved. The notification outbox drainer takes the same lock ID.</p><p>Every write records an intent, so the reaper has no enable switch. It is registered in <code>worker</code> and <code>all</code> mode, and is skipped only if it fails to build, which is logged as a warning.</p><p><a href="../background-services/">Background services coordination diagram &rarr;</a> &middot; <a href="../write-path/">Write path diagram &rarr;</a></p>'
    },
    DRAIN: {
      title: 'Drainer',
      badge: 'background', badgeText: 'background worker',
      body: '<p>Works backend drains. A drain is a <code>backend_drains</code> row, the durable record of the drain\'s state, created through the admin API (<code>POST /admin/api/backends/{name}/drain</code>). Cancelling the drain clears the row.</p><p>Runs every 10s under advisory lock <code>LockDrain</code> (1006). Each pass moves a draining backend\'s objects to other backends until the drain finishes or stalls. A backend with a drain record accepts no new writes, because <code>backend_capacity</code> marks it as not accepting writes.</p><p><a href="../background-services/">Background services coordination diagram &rarr;</a></p>'
    },
    SCRUB: {
      title: 'Scrubber',
      badge: 'background', badgeText: 'background worker',
      body: '<p>Integrity verification. Reads a batch of objects that have stored SHA-256 hashes, undoes encryption and compression, and checks the content still matches. Corrupted copies are enqueued for cleanup.</p><p>Runs only when <code>integrity.enabled</code> is true and <code>integrity.scrubber_interval</code> is set, under advisory lock <code>LockScrubber</code> (1010). A copy verified within <code>integrity.scrubber_min_age</code> (default 24h) is skipped.</p><p><a href="../background-services/">Background services coordination diagram &rarr;</a></p>'
    },
    RECON: {
      title: 'Reconciler',
      badge: 'background', badgeText: 'background worker',
      body: '<p>Scans each backend\'s bucket and imports objects the metadata store does not track, such as orphans from failed writes or manual uploads, so quota accounting stays accurate.</p><p>Registered only when <code>reconcile.enabled</code> is true. Runs every <code>reconcile.interval</code> (default 24h) under advisory lock <code>LockReconcile</code> (1009).</p><p><a href="../background-services/">Background services coordination diagram &rarr;</a></p>'
    },
    PROM: {
      title: 'Prometheus Metrics',
      badge: 'observability', badgeText: 'observability',
      body: '<p>Exposed at <code>/metrics</code>. Counters: requests, failovers, rejections, replication ops, cleanup ops, encryption ops. Gauges: quota (used/limit/free per backend), object counts, queue depth, build info. Histograms: request duration, request/response sizes, backend latency.</p><p>Per-backend metrics with <code>backend</code> label. Circuit breaker state as gauge (0=closed, 1=open, 2=half-open).</p>'
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
  };

  var tooltip = document.getElementById('ac-tooltip');
  var mouseX = 0, mouseY = 0;

  var pinned = false;
  document.addEventListener('mousemove', function(e) {
    mouseX = e.clientX;
    mouseY = e.clientY;
    if (tooltip.style.display === 'block' && !pinned) positionTooltip();
  });

  function positionTooltip() {
    var pad = 12;
    var w = tooltip.offsetWidth, h = tooltip.offsetHeight;
    var vw = window.innerWidth, vh = window.innerHeight;

    var x = mouseX + pad;
    if (x + w > vw - pad) x = mouseX - w - pad;
    x = Math.max(pad, Math.min(x, vw - w - pad));

    // Prefer below the cursor, and flip above only when above genuinely has
    // more room. Clamping afterwards is what keeps a tall panel on screen: an
    // unclamped flip puts its top edge above the viewport, and a panel taller
    // than the viewport pins to the top and scrolls instead.
    var below = vh - mouseY - pad * 2;
    var above = mouseY - pad * 2;
    var y = (h <= below || below >= above) ? mouseY + pad : mouseY - h - pad;
    y = Math.max(pad, Math.min(y, vh - h - pad));

    tooltip.style.left = x + 'px';
    tooltip.style.top = y + 'px';
  }

  function showInfo(id) {
    var info = nodeInfo[id];
    if (!info) { tooltip.style.display = 'none'; pinned = false; return; }
    tooltip.innerHTML = '<h3>' + info.title + '</h3><span class="ac-badge ac-badge-' + info.badge + '">' + info.badgeText + '</span>' + info.body;
    pinned = false;
    tooltip.style.display = 'block';
    positionTooltip();
    // Pin in place if tooltip has clickable links
    if (tooltip.querySelector('a')) pinned = true;
  }

  var hideTimer = null;
  var hoveringTooltip = false;
  var hoveringNode = false;

  tooltip.addEventListener('mouseenter', function() { hoveringTooltip = true; clearTimeout(hideTimer); });
  tooltip.addEventListener('mouseleave', function() {
    hoveringTooltip = false;
    hideTimer = setTimeout(function() {
      if (!hoveringNode && !hoveringTooltip) clearInfo();
    }, 100);
  });

  function clearInfo() {
    tooltip.style.display = 'none';
    pinned = false;
    var svg = document.querySelector('#ac-diagram svg');
    if (svg) {
      svg.classList.remove('highlighting');
      svg.querySelectorAll('.highlight').forEach(function(el) { el.classList.remove('highlight'); });
    }
  }

  function wireUpInteractivity() {
    var svg = document.querySelector('#ac-diagram svg');
    if (!svg) return;

    var adj = {}, edgeMap = {};
    svg.querySelectorAll('.edgePath').forEach(function(ep, i) {
      var cls = ep.getAttribute('class') || '';
      var m = cls.match(/LS-(\S+)/), m2 = cls.match(/LE-(\S+)/);
      if (!m || !m2) return;
      var from = m[1], to = m2[1];
      edgeMap[i] = { from: from, to: to, path: ep, label: svg.querySelectorAll('.edgeLabel')[i] };
      (adj[from] = adj[from] || []).push(i);
    });

    function bfs(startId, adjacency, getNext) {
      var visited = new Set([startId]), edges = new Set(), queue = [startId];
      while (queue.length) {
        var cur = queue.shift();
        (adjacency[cur] || []).forEach(function(ei) {
          edges.add(ei);
          var next = getNext(edgeMap[ei]);
          if (!visited.has(next)) { visited.add(next); queue.push(next); }
        });
      }
      return { nodes: visited, edges: edges };
    }

    var radj = {};
    Object.keys(edgeMap).forEach(function(i) {
      var e = edgeMap[i];
      (radj[e.to] = radj[e.to] || []).push(Number(i));
    });

    svg.querySelectorAll('.node').forEach(function(node) {
      var id = node.id.replace(/^flowchart-/, '').replace(/-\d+$/, '');

      node.addEventListener('mouseenter', function() {
        hoveringNode = true;
        clearTimeout(hideTimer);
        svg.classList.add('highlighting');
        var fwd = bfs(id, adj, function(e) { return e.to; });
        var bwd = bfs(id, radj, function(e) { return e.from; });
        var allNodes = new Set([...fwd.nodes, ...bwd.nodes]);
        var allEdges = new Set([...fwd.edges, ...bwd.edges]);

        svg.querySelectorAll('.node').forEach(function(n) {
          var nid = n.id.replace(/^flowchart-/, '').replace(/-\d+$/, '');
          n.classList.toggle('highlight', allNodes.has(nid));
        });
        Object.keys(edgeMap).forEach(function(i) {
          var hl = allEdges.has(Number(i));
          edgeMap[i].path.classList.toggle('highlight', hl);
          if (edgeMap[i].label) edgeMap[i].label.classList.toggle('highlight', hl);
        });
        showInfo(id);
      });

      node.addEventListener('mouseleave', function() {
        hoveringNode = false;
        hideTimer = setTimeout(function() {
          if (!hoveringNode && !hoveringTooltip) clearInfo();
        }, 100);
      });
    });
  }
})();
</script>

## Legend

| Color | Meaning |
|-------|---------|
| <span style="color:#1a7a5a">**Forest green**</span> | Entry point |
| <span style="color:#c4a35a">**Amber**</span> | Middleware / routing |
| <span style="color:#5ec9a0">**Teal**</span> | Request handlers |
| <span style="color:#4aaa8a">**Teal**</span> | Internal storage layer |
| <span style="color:#34b882">**Green**</span> | External data stores |
| <span style="color:#8a9aa8">**Gray**</span> | Background services |
| <span style="color:#d4a0a0">**Red**</span> | Observability |
