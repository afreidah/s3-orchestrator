---
title: "Write Path"
linkTitle: "Write Path"
weight: 2
---

Detailed flow of a PutObject request through backend selection, encryption, failover, and metadata recording. **Hover over any component** for implementation details.

<style>
  #ac-diagram { margin: 1rem 0; }
  #ac-tooltip {
    position: fixed; z-index: 9999;
    max-width: 380px; padding: 0.7rem 0.85rem;
    background: #161b22; border: 1px solid #30363d; border-radius: 6px;
    box-shadow: 0 4px 16px rgba(0,0,0,0.4); display: none;
  }
  #ac-tooltip a { color: #34b882; text-decoration: none; }
  #ac-tooltip a:hover { text-decoration: underline; }
  #ac-tooltip h3 { color: #2a9d73; font-size: 0.85rem; margin: 0 0 0.25rem 0; }
  #ac-tooltip .ac-badge {
    display: inline-block; padding: 1px 7px; border-radius: 4px;
    font-size: 0.6rem; font-weight: 600; margin-bottom: 0.4rem; text-transform: uppercase;
  }
  .ac-badge-entry { background: #1a7a5a22; color: #34b882; border: 1px solid #34b88255; }
  .ac-badge-filter { background: #6b5b2e22; color: #c4a35a; border: 1px solid #c4a35a55; }
  .ac-badge-decision { background: #2a9d7322; color: #2a9d73; border: 1px solid #2a9d7355; }
  .ac-badge-process { background: #2d7d6a22; color: #5ec9a0; border: 1px solid #5ec9a055; }
  .ac-badge-storage { background: #1a3a3022; color: #4aaa8a; border: 1px solid #4aaa8a55; }
  .ac-badge-success { background: #1a7a5a22; color: #34b882; border: 1px solid #34b88255; }
  .ac-badge-reject { background: #8b3a3a22; color: #d4a0a0; border: 1px solid #d4a0a055; }
  .ac-badge-cleanup { background: #4a556822; color: #8a9aa8; border: 1px solid #8a9aa855; }
  #ac-tooltip p { font-size: 0.75rem; line-height: 1.4; color: #c9d1d9; margin-bottom: 0.35rem; }
  #ac-tooltip code { background: #21262d; padding: 1px 4px; border-radius: 3px; font-size: 0.7rem; color: #4aaa8a; }
  #ac-tooltip .ac-metric { color: #a7d5c1; font-style: italic; font-size: 0.7rem; }
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
    '    PUT([PutObject\\nRequest]):::entry --> PREFLIGHT{CanAcceptWrite\\nPre-flight}:::filter',
    '    PREFLIGHT -->|no backends| R507[507 Insufficient\\nStorage]:::reject',
    '    PREFLIGHT -->|ok| FILTER[Filter Eligible\\nBackends]:::filter',
    '',
    '    FILTER --> USAGE[Usage Limits\\nCheck]:::filter',
    '    USAGE --> DRAIN[Exclude\\nDraining]:::filter',
    '    DRAIN --> HEALTH[Exclude\\nUnhealthy]:::filter',
    '',
    '    HEALTH -->|none eligible| R507B[507 Insufficient\\nStorage]:::reject',
    '    HEALTH -->|eligible > 0| BUFFER[Buffer Request\\nBody]:::process',
    '',
    '    BUFFER --> HASH{Integrity\\nEnabled?}:::decision',
    '    HASH -->|yes| COMPUTE[Compute SHA-256\\nContent Hash]:::process',
    '    HASH -->|no| SELECT',
    '    COMPUTE --> SELECT',
    '',
    '    SELECT{Select\\nBackend}:::decision',
    '    SELECT -->|spread| LEAST[Least Utilized\\nBackend]:::storage',
    '    SELECT -->|pack| FIRST[First With\\nSpace]:::storage',
    '    LEAST --> ENC',
    '    FIRST --> ENC',
    '',
    '    ENC{Encryption\\nEnabled?}:::decision',
    '    ENC -->|yes| ENCRYPT[Generate DEK\\nWrap + Encrypt]:::process',
    '    ENC -->|no| UPLOAD',
    '    ENCRYPT --> UPLOAD',
    '',
    '    UPLOAD[Upload to\\nBackend]:::process --> CB{Circuit\\nBreaker}:::decision',
    '    CB -->|open| FAIL',
    '    CB -->|closed/probe| S3[S3 Backend\\nPutObject]:::storage',
    '    S3 -->|error| FAIL{Upload\\nFailed?}:::decision',
    '    S3 -->|success| RECORD',
    '',
    '    FAIL -->|backends remain| RETRY[Remove Backend\\nfrom Eligible]:::process',
    '    RETRY --> SELECT',
    '    FAIL -->|all exhausted| RETERR[Return Last\\nError]:::reject',
    '',
    '    RECORD[Record Object\\nin PostgreSQL]:::storage --> DISPLACED{Displaced\\nCopies?}:::decision',
    '    DISPLACED -->|yes| CLEANUP[Delete Old Copies\\nor Enqueue Cleanup]:::cleanup',
    '    DISPLACED -->|no| CACHE',
    '    CLEANUP --> CACHE',
    '',
    '    CACHE[Invalidate\\nLocation Cache]:::process --> METRICS[Record Usage\\n& Metrics]:::process',
    '    METRICS --> OK[Return ETag\\n200 OK]:::success',
    '',
    '    classDef entry fill:#1a7a5a,stroke:#1a7a5a,color:#fff,font-weight:bold',
    '    classDef filter fill:#6b5b2e,stroke:#c4a35a,color:#fff',
    '    classDef decision fill:#1e2a26,stroke:#2a9d73,color:#e6edf3,font-size:11px',
    '    classDef process fill:#2d7d6a,stroke:#5ec9a0,color:#fff',
    '    classDef storage fill:#1a3a30,stroke:#4aaa8a,color:#c9d1d9',
    '    classDef success fill:#1a7a5a,stroke:#34b882,color:#fff,font-weight:bold',
    '    classDef reject fill:#8b3a3a,stroke:#d4a0a0,color:#fff,font-weight:bold',
    '    classDef cleanup fill:#222a26,stroke:#8a9aa8,color:#e6edf3'
  ].join('\n');

  mermaid.initialize({
    startOnLoad: false, theme: 'dark',
    flowchart: { nodeSpacing: 14, rankSpacing: 22, curve: 'basis', padding: 5, diagramPadding: 8, useMaxWidth: true }
  });

  mermaid.render('write-mermaid-svg', diagramSrc).then(function(result) {
    document.getElementById('ac-diagram').innerHTML = result.svg;
    wireUpInteractivity();
  });

  var nodeInfo = {
    PUT: {
      title: 'PutObject Request',
      badge: 'entry', badgeText: 'entry point',
      body: '<p>Incoming PUT request after passing through admission control, rate limiting, and SigV4 authentication.</p><p>At this point <code>Content-Length</code> and <code>MaxObjectSize</code> have already been validated by the HTTP handler. User metadata (<code>x-amz-meta-*</code>) has been extracted and validated (max 2KB total).</p>'
    },
    PREFLIGHT: {
      title: 'CanAcceptWrite Pre-flight',
      badge: 'filter', badgeText: 'early rejection',
      body: '<p><code>CanAcceptWrite(contentLength)</code> runs the full three-stage filter chain to check if <b>any</b> backend can accept this upload.</p><p>Called <b>before</b> reading the request body. With <code>Expect: 100-Continue</code>, Go\'s net/http delays the 100 Continue response until the first <code>Body.Read()</code>, so the client never transmits bytes for a doomed upload.</p><p class="ac-metric">Metric: s3o_early_rejections_total</p>'
    },
    R507: {
      title: '507 Insufficient Storage',
      badge: 'reject', badgeText: 'rejection',
      body: '<p>No backend can accept this upload. All backends are either over quota, draining, or have open circuit breakers.</p><p>Returned before body transmission, saving bandwidth for both client and server.</p>'
    },
    FILTER: {
      title: 'Filter Eligible Backends',
      badge: 'filter', badgeText: 'three-stage filter',
      body: '<p>Three nested filters applied in order: <code>excludeUnhealthy(excludeDraining(BackendsWithinLimits(order, 1, 0, size)))</code></p><p>Starts with the full backend order list and progressively narrows to only backends that can accept this write.</p>'
    },
    USAGE: {
      title: 'Usage Limits Check',
      badge: 'filter', badgeText: 'quota filter',
      body: '<p><code>BackendsWithinLimits(order, apiCalls=1, egress=0, ingress=size)</code></p><p>Checks three dimensions per backend against monthly rolling limits:</p><p>1. <b>API requests</b>: baseline + current + 1 &le; limit<br>2. <b>Egress bytes</b>: baseline + current + 0 &le; limit<br>3. <b>Ingress bytes</b>: baseline + current + size &le; limit</p><p>Effective usage = DB baseline (cached) + in-memory deltas (from counter backend). Orphan bytes from cleanup queue are factored into quota calculations.</p>'
    },
    DRAIN: {
      title: 'Exclude Draining',
      badge: 'filter', badgeText: 'drain filter',
      body: '<p>Removes backends marked for decommissioning via the admin drain API.</p><p>Checks <code>sync.Map</code> for each backend name. Draining backends accept no new writes while their existing objects are being migrated to other backends.</p>'
    },
    HEALTH: {
      title: 'Exclude Unhealthy',
      badge: 'filter', badgeText: 'health filter',
      body: '<p>Removes backends whose circuit breaker is <b>open</b> and <b>not probe-eligible</b> (timeout hasn\'t elapsed yet).</p><p>Probe-eligible backends (open + timeout elapsed) and half-open backends pass through, allowing the circuit breaker to test recovery on organic traffic.</p><p>Non-circuit-breaker backends always pass. Unknown backends are skipped.</p>'
    },
    R507B: {
      title: '507 Insufficient Storage',
      badge: 'reject', badgeText: 'rejection',
      body: '<p>After full filtering, no backends remain eligible. Returns <code>ErrInsufficientStorage</code>.</p><p class="ac-metric">Metric: s3o_usage_limit_rejections_total{operation="PutObject"}</p>'
    },
    BUFFER: {
      title: 'Buffer Request Body',
      badge: 'process', badgeText: 'buffering',
      body: '<p>Reads the entire request body into memory using <code>bufpool.Copy()</code> with pooled 32KB buffers from <code>sync.Pool</code>.</p><p>Necessary because <code>io.Reader</code> is single-use &mdash; if the upload fails and we need to retry on another backend, we need to replay the body. Each retry creates a fresh <code>bytes.NewReader(bodyBytes)</code>.</p><p>If encryption is enabled, each retry also re-encrypts with a fresh random DEK and nonce, producing different ciphertext.</p>'
    },
    HASH: {
      title: 'Integrity Enabled?',
      badge: 'decision', badgeText: 'branch',
      body: '<p>Checks if <code>integrity.enabled: true</code> in the hot-reloadable config.</p><p>If enabled, computes SHA-256 of the plaintext body before encryption. If disabled, skips directly to backend selection.</p>'
    },
    COMPUTE: {
      title: 'Compute SHA-256 Content Hash',
      badge: 'process', badgeText: 'integrity',
      body: '<p><code>HashBody(bodyBytes)</code> computes the SHA-256 hex digest of the buffered plaintext body.</p><p>The hash is stored in <code>object_locations.content_hash</code> alongside the object record. Used by read-time verification and the background scrubber to detect silent corruption.</p><p>Computed on plaintext before encryption so the same hash works for both encrypted and unencrypted objects.</p>'
    },
    SELECT: {
      title: 'Select Backend',
      badge: 'decision', badgeText: 'routing strategy',
      body: '<p>Chooses which backend to write to from the eligible list. Strategy is configured globally:</p><p><b>spread</b>: <code>GetLeastUtilizedBackend()</code> &mdash; picks the backend with the lowest utilization ratio (used/quota). Equalizes storage across all backends.</p><p><b>pack</b>: <code>GetBackendWithSpace()</code> &mdash; returns the first backend in order with sufficient free space. Consolidates storage, keeping later backends empty.</p>'
    },
    LEAST: {
      title: 'Least Utilized Backend',
      badge: 'storage', badgeText: 'DB query',
      body: '<p>PostgreSQL query: selects the backend from the eligible list with the lowest <code>used_bytes / quota_bytes</code> ratio that has at least <code>size</code> bytes free.</p><p>Returns <code>ErrNoSpaceAvailable</code> if no backend has sufficient space (translated to <code>ErrInsufficientStorage</code>).</p>'
    },
    FIRST: {
      title: 'First With Space',
      badge: 'storage', badgeText: 'DB query',
      body: '<p>PostgreSQL query: returns the first backend in the configured order that has at least <code>size</code> bytes of free quota.</p><p>Favors filling backends in order, which is useful for setups where you want to exhaust cheap/local storage before spilling to cloud backends.</p>'
    },
    ENC: {
      title: 'Encryption Enabled?',
      badge: 'decision', badgeText: 'branch',
      body: '<p>Checks if <code>o.encryptor != nil</code> (configured via <code>encryption.enabled: true</code>).</p><p>If disabled, the plaintext body is uploaded directly. If enabled, the body passes through the envelope encryption pipeline before upload.</p>'
    },
    ENCRYPT: {
      title: 'Generate DEK, Wrap + Encrypt',
      badge: 'process', badgeText: 'encryption',
      body: '<p>Envelope encryption pipeline:</p><p>1. Generate random 32-byte DEK (Data Encryption Key)<br>2. Wrap DEK with master key via <code>provider.WrapDEK(ctx, dek)</code> (Vault Transit or KMS)<br>3. Tee plaintext through MD5 hash (for ETag)<br>4. Stream encrypt with AES-256-GCM in chunks (default 1MB)</p><p>Produces <code>EncryptionMeta</code>: packed <code>baseNonce || wrappedDEK</code>, <code>keyID</code>, and <code>plaintextSize</code> stored in DB alongside the object record.</p><p class="ac-metric">Metric: s3o_encryption_ops_total{operation="encrypt"}</p>'
    },
    UPLOAD: {
      title: 'Upload to Backend',
      badge: 'process', badgeText: 'upload',
      body: '<p>Calls <code>backend.PutObject(ctx, key, body, size, contentType, metadata)</code> with an optional per-backend timeout (<code>backend_timeout</code> config).</p><p>The body is either plaintext (no encryption) or the ciphertext stream (encryption enabled). Size is ciphertext size when encrypted.</p>'
    },
    CB: {
      title: 'Circuit Breaker',
      badge: 'decision', badgeText: 'circuit breaker',
      body: '<p><code>CircuitBreakerBackend.PutObject()</code> wraps the real S3 call with <code>CBCall()</code>:</p><p><b>PreCheck</b>: if circuit is open and not probe-eligible, return <code>ErrBackendUnavailable</code> immediately without I/O.<br><b>On success</b>: if half-open, transition to closed (recovered).<br><b>On failure</b>: increment failure counter; if threshold reached, transition to open.</p><p><a href="../circuit-breaker/">Circuit breaker state machine diagram &rarr;</a></p>'
    },
    S3: {
      title: 'S3 Backend PutObject',
      badge: 'storage', badgeText: 'S3 API call',
      body: '<p>AWS SDK v2 <code>s3.PutObject()</code> call to the backend endpoint. Builds <code>PutObjectInput</code> with bucket, key, body, content-length, content-type, and user metadata.</p><p>Supports <code>unsignedPayload</code> mode for backends that accept unsigned streaming uploads (avoids buffering for SigV4 signing). Returns ETag on success.</p><p class="ac-metric">Metrics: s3o_backend_requests_total, s3o_backend_latency_seconds</p>'
    },
    FAIL: {
      title: 'Upload Failed?',
      badge: 'decision', badgeText: 'failover',
      body: '<p>On any backend error (network timeout, S3 error, circuit breaker rejection), the failed backend is recorded and removed from the eligible list.</p><p>Usage is still recorded for the failed API call (counts against monthly limits). The loop retries with the next eligible backend using a fresh <code>bytes.NewReader</code> from the buffered body.</p><p class="ac-metric">Metric: s3o_write_failover_total{operation, from_backend, to_backend}</p>'
    },
    RETRY: {
      title: 'Remove Backend from Eligible',
      badge: 'process', badgeText: 'failover',
      body: '<p>Removes the failed backend from the eligible list and logs a warning with the error, failed backend name, and count of remaining backends.</p><p>Loops back to backend selection, which will pick a different backend from the reduced eligible list. If encryption is enabled, a fresh DEK and nonce are generated for the retry.</p>'
    },
    RETERR: {
      title: 'Return Last Error',
      badge: 'reject', badgeText: 'failure',
      body: '<p>All eligible backends have been tried and failed. Returns the last error encountered, which propagates to the HTTP handler as a 502 Bad Gateway.</p><p>The span is marked with error status and the error is recorded for tracing.</p>'
    },
    RECORD: {
      title: 'Record Object in PostgreSQL',
      badge: 'storage', badgeText: 'DB transaction',
      body: '<p>Atomic database transaction (<code>RecordObject</code>):</p><p>1. <code>LockObjectKeyForWrite</code> &mdash; advisory lock for concurrent write safety<br>2. <code>GetExistingCopiesForUpdate</code> &mdash; SELECT FOR UPDATE on current copies<br>3. <code>DeleteObjectCopies</code> &mdash; remove all existing copies<br>4. <code>DecrementQuota</code> for each deleted copy<br>5. <code>InsertObjectLocation</code> &mdash; new record with encryption metadata<br>6. <code>IncrementQuota</code> for the new backend</p><p>Returns list of <b>displaced copies</b> on other backends that need cleanup.</p><p>If this transaction fails, the orphaned upload is deleted from the backend (or enqueued to cleanup queue if that also fails).</p>'
    },
    DISPLACED: {
      title: 'Displaced Copies?',
      badge: 'decision', badgeText: 'overwrite check',
      body: '<p>When overwriting an existing object, the old copies on <b>other</b> backends (replicas from replication) need to be cleaned up.</p><p><code>RecordObject</code> returns the list of displaced copies with their backend names and sizes. If the object didn\'t previously exist, this list is empty.</p>'
    },
    CLEANUP: {
      title: 'Delete Old Copies or Enqueue Cleanup',
      badge: 'cleanup', badgeText: 'cleanup',
      body: '<p>For each displaced copy on another backend:</p><p>1. Attempt immediate <code>backend.DeleteObject(ctx, key)</code><br>2. If delete fails: <code>enqueueCleanup()</code> &mdash; insert into <code>cleanup_queue</code> table with exponential backoff (1m to 24h, max 10 attempts)<br>3. <code>IncrementOrphanBytes()</code> on the backend\'s quota to prevent over-allocation while orphans exist</p><p>Audit event: <code>storage.overwrite_displaced</code> with count of displaced copies.</p><p class="ac-metric">Metric: s3o_cleanup_queue_enqueued_total{reason="overwrite_displaced"}</p>'
    },
    CACHE: {
      title: 'Invalidate Location Cache',
      badge: 'process', badgeText: 'cache',
      body: '<p><code>cache.Delete(key)</code> removes the cached backend location for this object key.</p><p>Ensures subsequent reads re-query the database to get the updated location, rather than reading from a stale cache entry pointing to the old backend.</p>'
    },
    METRICS: {
      title: 'Record Usage & Metrics',
      badge: 'process', badgeText: 'telemetry',
      body: '<p><code>Record(backendName, apiCalls=1, egress=0, ingress=size)</code> increments the monthly usage counters in the counter backend (local atomics or Redis).</p><p>Records operation duration histogram via <code>MetricsCollector</code>. If failover occurred, increments <code>WriteFailoverTotal</code> for each failed backend paired with the successful backend.</p><p>Audit event: <code>storage.PutObject</code> with key, backend name, plaintext size.</p>'
    },
    OK: {
      title: 'Return ETag / 200 OK',
      badge: 'success', badgeText: 'success',
      body: '<p>Returns the ETag from the successful backend upload. The HTTP handler sets the <code>ETag</code> response header and responds with <code>200 OK</code>.</p><p>If encryption is enabled, the ETag is the MD5 of the <b>plaintext</b> (computed during encryption via <code>io.TeeReader</code>) for S3 client compatibility.</p>'
    }
  };

  var tooltip = document.getElementById('ac-tooltip');
  var mouseX = 0, mouseY = 0;
  var pinned = false, hideTimer = null, hoveringTooltip = false, hoveringNode = false;

  tooltip.addEventListener('mouseenter', function() { hoveringTooltip = true; clearTimeout(hideTimer); });
  tooltip.addEventListener('mouseleave', function() {
    hoveringTooltip = false;
    hideTimer = setTimeout(function() { if (!hoveringNode && !hoveringTooltip) clearInfo(); }, 100);
  });

  document.addEventListener('mousemove', function(e) {
    mouseX = e.clientX; mouseY = e.clientY;
    if (tooltip.style.display === 'block' && !pinned) positionTooltip();
  });
  function positionTooltip() {
    var pad = 12, x = mouseX + pad, y = mouseY + pad;
    if (x + tooltip.offsetWidth > window.innerWidth - pad) x = mouseX - tooltip.offsetWidth - pad;
    if (y + tooltip.offsetHeight > window.innerHeight - pad) y = mouseY - tooltip.offsetHeight - pad;
    tooltip.style.left = x + 'px'; tooltip.style.top = y + 'px';
  }
  function showInfo(id) {
    var info = nodeInfo[id];
    if (!info) { tooltip.style.display = 'none'; pinned = false; return; }
    tooltip.innerHTML = '<h3>' + info.title + '</h3><span class="ac-badge ac-badge-' + info.badge + '">' + info.badgeText + '</span>' + info.body;
    pinned = false;
    tooltip.style.display = 'block'; positionTooltip();
    if (tooltip.querySelector('a')) pinned = true;
  }
  function clearInfo() {
    tooltip.style.display = 'none'; pinned = false;
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
      edgeMap[i] = { from: m[1], to: m2[1], path: ep, label: svg.querySelectorAll('.edgeLabel')[i] };
      (adj[m[1]] = adj[m[1]] || []).push(i);
    });
    function bfs(startId, adjacency, getNext) {
      var visited = new Set([startId]), edges = new Set(), queue = [startId];
      while (queue.length) { var cur = queue.shift(); (adjacency[cur] || []).forEach(function(ei) {
        edges.add(ei); var next = getNext(edgeMap[ei]);
        if (!visited.has(next)) { visited.add(next); queue.push(next); }
      }); } return { nodes: visited, edges: edges };
    }
    var radj = {};
    Object.keys(edgeMap).forEach(function(i) { var e = edgeMap[i]; (radj[e.to] = radj[e.to] || []).push(Number(i)); });
    svg.querySelectorAll('.node').forEach(function(node) {
      var id = node.id.replace(/^flowchart-/, '').replace(/-\d+$/, '');
      node.addEventListener('mouseenter', function() {
        hoveringNode = true; clearTimeout(hideTimer);
        svg.classList.add('highlighting');
        var fwd = bfs(id, adj, function(e) { return e.to; });
        var bwd = bfs(id, radj, function(e) { return e.from; });
        var allNodes = new Set([...fwd.nodes, ...bwd.nodes]);
        var allEdges = new Set([...fwd.edges, ...bwd.edges]);
        svg.querySelectorAll('.node').forEach(function(n) {
          n.classList.toggle('highlight', allNodes.has(n.id.replace(/^flowchart-/, '').replace(/-\d+$/, '')));
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
        hideTimer = setTimeout(function() { if (!hoveringNode && !hoveringTooltip) clearInfo(); }, 100);
      });
    });
  }
})();
</script>

## Legend

| Color | Meaning |
|-------|---------|
| <span style="color:#1a7a5a">**Forest green**</span> | Entry point |
| <span style="color:#c4a35a">**Amber**</span> | Eligibility filtering |
| <span style="color:#2a9d73">**Green border**</span> | Decision / branch |
| <span style="color:#5ec9a0">**Teal**</span> | Processing step |
| <span style="color:#4aaa8a">**Teal**</span> | Storage / DB / S3 |
| <span style="color:#34b882">**Green**</span> | Success |
| <span style="color:#d4a0a0">**Red**</span> | Rejection / failure |
| <span style="color:#8a9aa8">**Gray**</span> | Cleanup |
