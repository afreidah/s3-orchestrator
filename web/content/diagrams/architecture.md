---
title: "System Architecture"
linkTitle: "Architecture"
weight: -1
---

High-level architecture of the S3 Orchestrator showing the request path, storage layer, background services, and observability. **Hover over any component** for implementation details.

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

<script src="https://cdn.jsdelivr.net/npm/mermaid@11/dist/mermaid.min.js"></script>
<script>
(function() {
  var diagramSrc = [
    'flowchart TD',
    '    CLIENT([S3 Client]):::entry --> HTTP[HTTP Server\\nTLS / Timeouts]:::middleware',
    '    HTTP --> ADMIT[Admission Control\\n& Rate Limiter]:::middleware',
    '    ADMIT --> AUTH[SigV4 / Presigned / Token\\nAuthentication]:::middleware',
    '    AUTH --> ROUTE{Request\\nRouter}:::middleware',
    '',
    '    ROUTE -->|PUT/GET/HEAD/DELETE| OBJMGR[Object\\nManager]:::handler',
    '    ROUTE -->|multipart| MPMGR[Multipart\\nManager]:::handler',
    '    ROUTE -->|list/head bucket| BUCKETS[Bucket\\nOperations]:::handler',
    '    ROUTE -->|/admin/| ADMIN[Admin\\nAPI]:::handler',
    '    ROUTE -->|/ui/| WEBUI[Web\\nDashboard]:::handler',
    '',
    '    OBJMGR --> DCACHE[Object Data\\nCache]:::storage',
    '    DCACHE -->|miss| ENC{Encryption}:::storage',
    '    MPMGR --> ENC',
    '    ENC -->|enabled| VAULT[Key Provider\\nVault / KMS]:::data',
    '    ENC --> SELECT[Backend Selection\\n& Failover]:::storage',
    '',
    '    SELECT --> CB1[Circuit\\nBreaker]:::storage',
    '    CB1 --> BE1[S3 Backend 1]:::data',
    '    CB1 --> BE2[S3 Backend 2]:::data',
    '    CB1 --> BEN[S3 Backend N]:::data',
    '',
    '    OBJMGR --> CACHE[Location\\nCache]:::storage',
    '    CACHE --> DBCB[DB Circuit\\nBreaker]:::storage',
    '    MPMGR --> DBCB',
    '    BUCKETS --> DBCB',
    '    ADMIN --> OBJMGR',
    '    WEBUI --> OBJMGR',
    '    DBCB --> PG[(PostgreSQL)]:::data',
    '    DBCB -->|open| BROADCAST[Broadcast\\nReads]:::storage',
    '    BROADCAST --> CB1',
    '',
    '    USAGE[Usage Tracker\\n& Quota Enforcement]:::background',
    '    USAGE --> COUNTER[Counter Backend\\nLocal / Redis]:::data',
    '    USAGE --> PG',
    '    OBJMGR --> USAGE',
    '',
    '    REPL[Replicator]:::background --> SELECT',
    '    REPL --> PG',
    '    REBAL[Rebalancer]:::background --> SELECT',
    '    REBAL --> PG',
    '    CLEAN[Cleanup\\nQueue]:::background --> CB1',
    '    CLEAN --> PG',
    '    LIFE[Lifecycle\\nExpiration]:::background --> OBJMGR',
    '    MPCLEAN[Multipart\\nCleanup]:::background --> MPMGR',
    '    OVERREP[Over-Replication\\nCleaner]:::background --> SELECT',
    '    OVERREP --> PG',
    '',
    '    HTTP --> PROM[Prometheus\\nMetrics]:::observability',
    '    HTTP --> TEMPO[OpenTelemetry\\nTracing]:::observability',
    '    HTTP --> AUDIT[Structured\\nAudit Logs]:::observability',
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
    theme: 'dark',
    flowchart: { nodeSpacing: 14, rankSpacing: 22, curve: 'basis', padding: 5, diagramPadding: 8, useMaxWidth: true }
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
      body: '<p><b>Admission Control:</b> Channel-based semaphore limiting concurrent in-flight requests. Global pool or separate read/write pools. Probabilistic load shedding ramps rejection from <code>shed_threshold</code> to capacity. Optional brief wait before hard rejection.</p><p><b>Rate Limiter:</b> Per-IP token bucket using <code>golang.org/x/time/rate</code>. Extracts real client IP via X-Forwarded-For with trusted proxy CIDR validation.</p><p><a href="../admission-control/" style="color:#34b882">See detailed admission control flow diagram &rarr;</a></p>'
    },
    AUTH: {
      title: 'SigV4 / Presigned / Token Authentication',
      badge: 'middleware', badgeText: 'authentication',
      body: '<p>Verifies AWS Signature Version 4 from either the <code>Authorization</code> header or presigned URL query parameters. Reconstructs canonical request, derives signing key via HMAC-SHA256 chain, compares with <code>crypto/subtle.ConstantTimeCompare</code>.</p><p>Signing keys cached in <code>sync.Map</code>. Presigned URLs validated via <code>X-Amz-Expires</code> (max 7 days). Also supports legacy <code>X-Proxy-Token</code> header.</p><p><code>BucketRegistry</code> maps access keys to virtual buckets for multi-tenant credential isolation.</p>'
    },
    ROUTE: {
      title: 'Request Router',
      badge: 'middleware', badgeText: 'dispatcher',
      body: '<p>Dispatches by HTTP method, path, and query parameters:</p><p><b>Objects:</b> PUT, GET, HEAD, DELETE, CopyObject<br><b>Multipart:</b> CreateUpload, UploadPart, Complete, Abort, ListParts<br><b>Buckets:</b> ListObjects (v1/v2), HeadBucket, ListBuckets<br><b>Batch:</b> DeleteObjects (up to 1000 keys)<br><b>Admin:</b> /admin/* endpoints<br><b>UI:</b> /ui/* dashboard</p>'
    },
    OBJMGR: {
      title: 'Object Manager',
      badge: 'handler', badgeText: 'core handler',
      body: '<p>CRUD operations with automatic failover. <b>PutObject</b>: filters backends by quota/health/draining, selects via routing strategy (spread/pack), retries on failure. <b>GetObject</b>: checks location cache, queries DB, streams from backend with failover to replicas. Supports Range requests and conditional headers.</p><p><b>DeleteObject</b>: removes from all backends + DB metadata. Failed deletions enqueued to <code>cleanup_queue</code>.</p><p><b>CanAcceptWrite</b>: pre-flight check before body transmission (Expect: 100-Continue).</p><p><a href="../write-path/">Write path diagram &rarr;</a> · <a href="../read-path/">Read path diagram &rarr;</a></p>'
    },
    MPMGR: {
      title: 'Multipart Manager',
      badge: 'handler', badgeText: 'handler',
      body: '<p>Manages multipart upload lifecycle. Parts stored at temporary keys (<code>__multipart/{uploadID}/{partNumber}</code>) until completion.</p><p><b>Create</b>: selects backend, generates uploadID, records in DB.<br><b>UploadPart</b>: encrypts if enabled, stores part, records in DB.<br><b>Complete</b>: assembles final object, cleans up temporary parts.<br><b>Abort</b>: deletes temporary parts + DB records.</p>'
    },
    BUCKETS: {
      title: 'Bucket Operations',
      badge: 'handler', badgeText: 'handler',
      body: '<p><b>ListObjects</b> (v1 and v2): paginated listing from PostgreSQL with prefix filtering, delimiter support, and CommonPrefixes for directory simulation.</p><p><b>HeadBucket</b>: returns 200 if bucket exists (credential validation). <b>ListBuckets</b>: returns authorized bucket. <b>GetBucketVersioning</b>: always returns disabled (not supported).</p>'
    },
    ADMIN: {
      title: 'Admin API',
      badge: 'handler', badgeText: 'handler',
      body: '<p>Operational control endpoints at <code>/admin/api/*</code>. Protected by <code>X-Admin-Token</code> header.</p><p><b>Triggers:</b> flush-usage, rebalance, replicate, cleanup-queue, encrypt-existing.<br><b>Monitoring:</b> health, config, dashboard stats, object listing.<br><b>Drain:</b> start/check/cancel backend decommissioning (moves all objects to other backends).</p>'
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
    ENC: {
      title: 'Encryption Layer',
      badge: 'storage', badgeText: 'optional',
      body: '<p>Transparent envelope encryption when <code>encryption.enabled: true</code>.</p><p><b>Write:</b> generate random 256-bit DEK &rarr; wrap with master key &rarr; AES-256-GCM stream encrypt (1MB chunks) &rarr; store ciphertext + wrapped DEK in DB.</p><p><b>Read:</b> unwrap DEK &rarr; stream decrypt. <b>Range reads:</b> calculate affected chunks, fetch and decrypt only those.</p><p>ETag is MD5 of plaintext for S3 client compatibility.</p><p><a href="../encryption/">Encryption flow diagram &rarr;</a></p>'
    },
    VAULT: {
      title: 'Key Provider (Vault / KMS)',
      badge: 'data', badgeText: 'external',
      body: '<p>Master key management for envelope encryption. Wraps/unwraps per-object Data Encryption Keys (DEKs).</p><p>Supports HashiCorp Vault Transit engine or cloud KMS. Key rotation supported with zero-downtime via key versioning &mdash; old DEKs remain decryptable.</p>'
    },
    SELECT: {
      title: 'Backend Selection & Failover',
      badge: 'storage', badgeText: 'routing',
      body: '<p>Selects target backend using configured strategy:</p><p><b>spread:</b> picks least-utilized backend (equalizes storage across backends).<br><b>pack:</b> fills backends in order (consolidates storage, frees later backends).</p><p><b>Write failover:</b> on backend failure, removes from eligible list and retries next backend. <b>Read failover:</b> tries all replicas until one succeeds.</p><p><code>excludeUnhealthy()</code> filters circuit-broken backends but allows probe-eligible ones through to prevent deadlock.</p>'
    },
    CB1: {
      title: 'Backend Circuit Breakers',
      badge: 'storage', badgeText: 'resilience',
      body: '<p>Per-backend three-state circuit breaker: <b>closed</b> (healthy) &rarr; <b>open</b> (after N consecutive failures) &rarr; <b>half-open</b> (probe after timeout) &rarr; <b>closed</b>.</p><p>When open, returns <code>ErrBackendUnavailable</code> immediately without I/O. Probe-eligible backends (open + timeout elapsed) allowed through to prevent deadlock when all backends trip.</p><p>Config: <code>circuit_breaker.failure_threshold</code>, <code>circuit_breaker.open_timeout</code>.</p><p class="ac-metric">Metrics: s3o_circuit_breaker_state, s3o_circuit_breaker_transitions_total</p><p><a href="../circuit-breaker/">Circuit breaker state machine diagram &rarr;</a></p>'
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
      body: '<p>In-memory cache mapping object keys to their backend locations. Avoids a DB query on every read.</p><p>TTL-based eviction with background cleanup goroutine. Invalidated on write (PutObject, DeleteObject, CopyObject) to ensure consistency.</p><p>Cache miss falls through to PostgreSQL query.</p>'
    },
    DBCB: {
      title: 'DB Circuit Breaker',
      badge: 'storage', badgeText: 'resilience',
      body: '<p>Circuit breaker wrapping the PostgreSQL metadata store. Detects DB outages and returns <code>ErrDBUnavailable</code> sentinel.</p><p>When open, the ObjectManager triggers <b>broadcast reads</b>: parallel fan-out to all backends, returning the first successful response. Allows reads to continue during DB downtime.</p><p>Only actual DB errors trip the breaker; application-level errors (ErrNoSpaceAvailable, S3Error) are excluded.</p><p><a href="../circuit-breaker/">Circuit breaker state machine diagram &rarr;</a></p>'
    },
    PG: {
      title: 'PostgreSQL',
      badge: 'data', badgeText: 'metadata store',
      body: '<p>Stores all object metadata, locations, multipart state, quotas, usage counters, cleanup queue, and replication state.</p><p>Tables: <code>objects</code>, <code>multipart_uploads</code>, <code>multipart_parts</code>, <code>backend_quotas</code>, <code>backend_usage</code>, <code>cleanup_queue</code>, <code>replication_state</code>.</p><p>Uses advisory locks for distributed worker coordination. Connection pool: pgx with configurable <code>max_conns</code>, <code>min_conns</code>, <code>max_conn_lifetime</code>. Migrations applied automatically on startup.</p>'
    },
    BROADCAST: {
      title: 'Broadcast Reads',
      badge: 'storage', badgeText: 'fallback',
      body: '<p>Degraded-mode read path activated when the DB circuit breaker is open. Sends parallel GET requests to all backends simultaneously.</p><p>Returns the first successful response, cancels remaining in-flight requests. Allows reads to continue during PostgreSQL outages as long as at least one backend holds the object.</p><p><a href="../read-path/">Read path diagram &rarr;</a></p>'
    },
    USAGE: {
      title: 'Usage Tracker & Quota Enforcement',
      badge: 'background', badgeText: 'quota',
      body: '<p>Tracks per-backend monthly counters: API requests, egress bytes, ingress bytes. Effective usage = DB baseline + unflushed in-memory deltas.</p><p><code>BackendsWithinLimits()</code> filters backends exceeding their configured limits before every write. Flushes deltas to DB every 30s (adaptive: 10s when near limit).</p><p>With Redis: shared counters across instances, advisory lock prevents destructive concurrent flushes.</p>'
    },
    COUNTER: {
      title: 'Counter Backend',
      badge: 'data', badgeText: 'counters',
      body: '<p><b>Local:</b> atomic in-process counters (single instance).<br><b>Redis:</b> shared counters for multi-instance deployments.</p><p>Redis fallback: if Redis is down, counters accumulate locally and flush to DB on the next successful cycle. No data loss, just temporary per-instance isolation.</p>'
    },
    REPL: {
      title: 'Replicator',
      badge: 'background', badgeText: 'background worker',
      body: '<p>Creates additional copies of under-replicated objects. Runs every 5 minutes under advisory lock (<code>LockReplicator</code>).</p><p>Queries DB for objects with fewer copies than <code>replication.factor</code>. Stream-copies to backends with available quota (binary copy, no re-encryption). Skips backends that have been unhealthy longer than <code>unhealthy_threshold</code>.</p><p><a href="../background-services/">Background services coordination diagram &rarr;</a></p>'
    },
    REBAL: {
      title: 'Rebalancer',
      badge: 'background', badgeText: 'background worker',
      body: '<p>Redistributes objects across backends to optimize storage utilization. Runs every 6 hours under advisory lock.</p><p><b>spread:</b> equalizes utilization ratios across backends.<br><b>pack:</b> consolidates objects into fewer backends, freeing up others.</p><p>Moves objects: GET from source &rarr; PUT to target (binary stream copy, no re-encryption) &rarr; delete from source &rarr; update DB.</p><p><a href="../background-services/">Background services coordination diagram &rarr;</a></p>'
    },
    CLEAN: {
      title: 'Cleanup Queue',
      badge: 'background', badgeText: 'background worker',
      body: '<p>Retries failed backend deletions with exponential backoff (1 minute to 24 hours, max 10 attempts). Runs every minute, processes up to 50 items with 10 concurrent goroutines.</p><p>Enqueued at all failure sites: PutObject rollback, DeleteObject, multipart abort/complete, rebalancer, replicator.</p><p class="ac-metric">Metrics: s3o_cleanup_queue_depth, s3o_cleanup_queue_processed_total</p><p><a href="../background-services/">Background services coordination diagram &rarr;</a></p>'
    },
    LIFE: {
      title: 'Lifecycle Expiration',
      badge: 'background', badgeText: 'background worker',
      body: '<p>Automatic object expiration with configurable prefix rules. Runs hourly under advisory lock.</p><p>Example: <code>prefix: temp/, days: 30</code> deletes all objects under <code>temp/</code> older than 30 days. Calls ObjectManager.DeleteObject for proper cleanup across all backends.</p><p><a href="../background-services/">Background services coordination diagram &rarr;</a></p>'
    },
    MPCLEAN: {
      title: 'Multipart Cleanup',
      badge: 'background', badgeText: 'background worker',
      body: '<p>Finds incomplete multipart uploads older than 24 hours and aborts them. Runs hourly under advisory lock.</p><p>Prevents orphaned temporary parts (<code>__multipart/{uploadID}/*</code>) from accumulating on backends when clients abandon uploads.</p><p><a href="../background-services/">Background services coordination diagram &rarr;</a></p>'
    },
    OVERREP: {
      title: 'Over-Replication Cleaner',
      badge: 'background', badgeText: 'background worker',
      body: '<p>Finds objects with more copies than the configured replication factor and deletes excess replicas. Runs every 6 hours under advisory lock.</p><p>Preserves geographically diverse copies when possible. Frees up quota on backends holding unnecessary duplicates.</p><p><a href="../background-services/">Background services coordination diagram &rarr;</a></p>'
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
    var x = mouseX + pad, y = mouseY + pad;
    if (x + tooltip.offsetWidth > window.innerWidth - pad) x = mouseX - tooltip.offsetWidth - pad;
    if (y + tooltip.offsetHeight > window.innerHeight - pad) y = mouseY - tooltip.offsetHeight - pad;
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
