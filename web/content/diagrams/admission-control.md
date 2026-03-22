---
title: "Admission Control Flow"
linkTitle: "Admission Control"
weight: 1
---

This interactive diagram shows the complete request lifecycle through the S3 Orchestrator's admission control pipeline. **Hover over any node** to highlight its upstream/downstream path and see implementation details.

<style>
  #ac-diagram { margin: 1rem 0; }

  /* floating tooltip */
  #ac-tooltip {
    position: fixed; z-index: 9999;
    max-width: 360px; padding: 0.7rem 0.85rem;
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
  .ac-badge-decision { background: #2a9d7322; color: #2a9d73; border: 1px solid #2a9d7355; }
  .ac-badge-reject { background: #8b3a3a22; color: #d4a0a0; border: 1px solid #d4a0a055; }
  .ac-badge-success { background: #1a7a5a22; color: #34b882; border: 1px solid #34b88255; }
  .ac-badge-pool { background: #1a3a3022; color: #4aaa8a; border: 1px solid #4aaa8a55; }
  .ac-badge-process { background: #4a556822; color: #8a9aa8; border: 1px solid #8a9aa855; }
  .ac-badge-write { background: #2d7d6a22; color: #5ec9a0; border: 1px solid #5ec9a055; }
  .ac-badge-read { background: #1a7a5a22; color: #34b882; border: 1px solid #34b88255; }
  .ac-badge-cb { background: #6b5b2e22; color: #c4a35a; border: 1px solid #c4a35a55; }
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
    '    REQ([S3 Request]):::start --> AC{Admission\\nController}:::decision',
    '    AC -->|disabled| RL',
    '    AC -->|global pool| GSEM[Global\\nSemaphore]:::pool',
    '    AC -->|split pools| SPLIT{Write?}:::decision',
    '    SPLIT -->|PUT/POST/DEL| WSEM[Write Sem]:::pool',
    '    SPLIT -->|GET/HEAD| RSEM[Read Sem]:::pool',
    '    GSEM --> SHED{Load Shed\\nPressure}:::decision',
    '    WSEM --> SHED',
    '    RSEM --> SHED',
    '    SHED -->|shed ramp| R503S[503 SlowDown\\nShed]:::reject',
    '    SHED -->|ok| TRY{Acquire\\nSlot}:::decision',
    '    TRY -->|got slot| RL',
    '    TRY -->|full| WAIT{Brief\\nWait}:::decision',
    '    WAIT -->|acquired| RL',
    '    WAIT -->|timeout| R503H[503 SlowDown\\nHard]:::reject',
    '    RL{Rate\\nLimiter}:::decision',
    '    RL -->|disabled or ok| AUTH',
    '    RL -->|exceeded| R429[429 SlowDown]:::reject',
    '    AUTH[SigV4 / Token\\nAuth]:::process --> AUTHOK{Valid?}:::decision',
    '    AUTHOK -->|no| R403[403 Denied]:::reject',
    '    AUTHOK -->|yes| ROUTE{Method\\nRouting}:::decision',
    '    ROUTE -->|PUT| SZ{Size\\nCheck}:::decision',
    '    SZ -->|no Content-Length| R411[411]:::reject',
    '    SZ -->|too large| R413[413]:::reject',
    '    SZ -->|ok| CAP{Backend\\nCapacity?}:::decision',
    '    CAP -->|none available| R507[507 No Space]:::reject',
    '    CAP -->|ok, 100-Continue| WRITE[PutObject]:::write',
    '    ROUTE -->|GET/HEAD| READ[GetObject\\nHeadObject]:::read',
    '    ROUTE -->|DELETE| DEL[DeleteObject]:::write',
    '    WRITE --> CBW{Circuit\\nBreaker}:::cb',
    '    READ --> CBR{Circuit\\nBreaker}:::cb',
    '    DEL --> CBD{Circuit\\nBreaker}:::cb',
    '    CBW -->|open| R502W[502]:::reject',
    '    CBW -->|ok/probe| OK_W[200 OK]:::success',
    '    CBR -->|failover| CBR',
    '    CBR -->|ok/probe| OK_R[200 / 206]:::success',
    '    CBD -->|open| R502D[502]:::reject',
    '    CBD -->|ok/probe| OK_D[204]:::success',
    '    classDef start fill:#1a7a5a,stroke:#1a7a5a,color:#fff,font-weight:bold',
    '    classDef decision fill:#1e2a26,stroke:#2a9d73,color:#e6edf3,font-size:11px',
    '    classDef process fill:#1c2128,stroke:#8b949e,color:#e6edf3',
    '    classDef pool fill:#1a3a30,stroke:#4aaa8a,color:#c9d1d9',
    '    classDef reject fill:#8b3a3a,stroke:#d4a0a0,color:#fff,font-weight:bold',
    '    classDef success fill:#1a7a5a,stroke:#34b882,color:#fff,font-weight:bold',
    '    classDef write fill:#2d7d6a,stroke:#5ec9a0,color:#fff',
    '    classDef read fill:#1a7f37,stroke:#3fb950,color:#fff',
    '    classDef cb fill:#6b5b2e,stroke:#c4a35a,color:#fff'
  ].join('\n');

  mermaid.initialize({
    startOnLoad: false,
    theme: 'dark',
    flowchart: { nodeSpacing: 14, rankSpacing: 22, curve: 'basis', padding: 5, diagramPadding: 8, useMaxWidth: true }
  });

  mermaid.render('ac-mermaid-svg', diagramSrc).then(function(result) {
    document.getElementById('ac-diagram').innerHTML = result.svg;
    wireUpInteractivity();
  });

  var nodeInfo = {
    REQ: {
      title: 'S3 Request',
      badge: 'process', badgeText: 'entry point',
      body: '<p>Incoming HTTP request to the S3-compatible API endpoint.</p><p>Carries AWS SigV4 <code>Authorization</code> header (or legacy <code>X-Proxy-Token</code>), HTTP method, and <code>/{bucket}/{key}</code> path.</p><p>A unique <code>X-Amz-Request-Id</code> is assigned (or honoured from an upstream <code>X-Request-Id</code> header) for audit correlation through the entire pipeline.</p>'
    },
    AC: {
      title: 'Admission Controller',
      badge: 'decision', badgeText: 'middleware',
      body: '<p>Channel-based semaphore that caps concurrent in-flight operations. Prevents backend overload under burst traffic.</p><p>Three modes: <b>disabled</b> (passthrough), <b>global pool</b> (single <code>chan struct{}</code> for all ops), or <b>split pools</b> (separate read/write channels).</p><p>The semaphore is shared between HTTP requests and background services (rebalancer, replicator, over-replication cleaner, cleanup worker), so <code>max_concurrent_requests</code> is the total budget for all backend operations.</p><p>Config: <code>admission.max_concurrent</code> (global) or <code>admission.max_reads</code> / <code>admission.max_writes</code> (split).</p>'
    },
    GSEM: {
      title: 'Global Semaphore',
      badge: 'pool', badgeText: 'concurrency pool',
      body: '<p>Single buffered channel shared by all request types (GET, PUT, DELETE, HEAD, POST).</p><p>Capacity set by <code>admission.max_concurrent</code>. Each in-flight request holds one slot; released on handler return via <code>defer</code>.</p>'
    },
    SPLIT: {
      title: 'Write Check',
      badge: 'decision', badgeText: 'router',
      body: '<p>Routes to the correct pool based on HTTP method.</p><p><b>Write</b>: PUT, POST, DELETE &rarr; write semaphore<br><b>Read</b>: GET, HEAD &rarr; read semaphore</p><p>Isolates write load so heavy uploads can\'t starve reads.</p>'
    },
    WSEM: {
      title: 'Write Semaphore',
      badge: 'pool', badgeText: 'concurrency pool',
      body: '<p>Dedicated buffered channel for write operations (PUT/POST/DELETE).</p><p>Capacity set by <code>admission.max_writes</code>. Prevents write storms from consuming all server resources.</p>'
    },
    RSEM: {
      title: 'Read Semaphore',
      badge: 'pool', badgeText: 'concurrency pool',
      body: '<p>Dedicated buffered channel for read operations (GET/HEAD).</p><p>Capacity set by <code>admission.max_reads</code>. Ensures reads remain responsive even under heavy write load.</p>'
    },
    SHED: {
      title: 'Load Shed Pressure',
      badge: 'decision', badgeText: 'probabilistic',
      body: '<p>Probabilistic early rejection before the hard concurrency limit. When pool occupancy exceeds <code>shed_threshold</code> (e.g. 0.8 = 80%), rejection probability ramps <b>linearly</b> from 0% at threshold to 100% at full capacity.</p><p>Formula: <code>p = (occupancy - threshold) / (capacity - threshold)</code></p><p>Uses <code>math/rand.Float64()</code> coin flip. Set <code>shed_threshold: 0</code> to disable.</p>'
    },
    R503S: {
      title: '503 SlowDown (Shed)',
      badge: 'reject', badgeText: 'rejection',
      body: '<p>Active load shedding rejection. The probabilistic ramp decided to shed this request before the hard limit.</p><p>Response: <code>503 Service Unavailable</code> with <code>Retry-After: 1</code> and S3-formatted XML error body (<code>SlowDown</code>).</p><p class="ac-metric">Metric: s3o_load_shed_total</p>'
    },
    TRY: {
      title: 'Acquire Slot',
      badge: 'decision', badgeText: 'non-blocking',
      body: '<p>Non-blocking <code>select</code> on the semaphore channel. If a slot is immediately available, the request passes through and the slot is held until the handler returns.</p><p>If the channel is full, falls through to the wait path (or instant rejection if <code>admission_wait</code> is 0).</p>'
    },
    WAIT: {
      title: 'Brief Wait',
      badge: 'decision', badgeText: 'timed wait',
      body: '<p>When the semaphore is full, waits up to <code>admission_wait</code> duration for a slot to open.</p><p>Uses <code>time.NewTimer</code> with a three-way <code>select</code>: semaphore acquired, timer expired, or request context cancelled.</p><p>Default <code>admission_wait: 0</code> means instant rejection (no wait).</p>'
    },
    R503H: {
      title: '503 SlowDown (Hard)',
      badge: 'reject', badgeText: 'rejection',
      body: '<p>Hard admission rejection: the wait timer expired without acquiring a semaphore slot, or the client disconnected.</p><p>Response: <code>503 Service Unavailable</code> with <code>Retry-After: 1</code>.</p><p class="ac-metric">Metric: s3o_admission_rejections_total</p>'
    },
    RL: {
      title: 'Rate Limiter',
      badge: 'decision', badgeText: 'middleware',
      body: '<p>Per-IP token bucket rate limiter using <code>golang.org/x/time/rate</code>.</p><p>Extracts client IP from <code>RemoteAddr</code> or <code>X-Forwarded-For</code> (rightmost untrusted IP when request arrives from a trusted proxy CIDR).</p><p>Config: <code>rate_limit.requests_per_sec</code>, <code>rate_limit.burst</code>, <code>rate_limit.trusted_proxies</code>.</p><p>Background goroutine cleans stale per-IP entries every <code>cleanup_interval</code> (default 1m). Disabled when <code>rate_limit</code> section is absent.</p>'
    },
    R429: {
      title: '429 SlowDown',
      badge: 'reject', badgeText: 'rejection',
      body: '<p>Per-IP rate limit exceeded. The token bucket for this client IP is empty.</p><p>Response: <code>429 Too Many Requests</code> with <code>Retry-After: 1</code>.</p><p class="ac-metric">Metric: s3o_rate_limit_rejections_total</p>'
    },
    AUTH: {
      title: 'SigV4 / Token Auth',
      badge: 'process', badgeText: 'authentication',
      body: '<p>Verifies AWS Signature Version 4: parses the <code>Authorization</code> header, reconstructs the canonical request + string-to-sign, and derives the signing key via the HMAC-SHA256 chain:</p><p><code>secret &rarr; dateKey &rarr; dateRegionKey &rarr; dateRegionServiceKey &rarr; signingKey</code></p><p>Signing keys are cached in a <code>sync.Map</code> keyed by <code>accessKey/dateStamp/region/service</code>. Clock skew tolerance: &plusmn;15 minutes.</p><p>Also supports legacy <code>X-Proxy-Token</code> header for simple clients.</p>'
    },
    AUTHOK: {
      title: 'Signature Valid?',
      badge: 'decision', badgeText: 'verification',
      body: '<p>Compares the computed signature against the request\'s signature using <code>crypto/subtle.ConstantTimeCompare</code> (timing-safe).</p><p>On success, resolves the access key &rarr; virtual bucket via <code>BucketRegistry</code>, enabling multi-tenant per-bucket credential isolation.</p>'
    },
    R403: {
      title: '403 Access Denied',
      badge: 'reject', badgeText: 'rejection',
      body: '<p>Authentication failure. Either <code>SignatureDoesNotMatch</code> (bad secret / tampered request) or <code>InvalidAccessKeyId</code> (unknown credentials).</p>'
    },
    ROUTE: {
      title: 'Method Routing',
      badge: 'decision', badgeText: 'dispatcher',
      body: '<p>Dispatches to the appropriate handler based on HTTP method and path:</p><p><b>PUT</b> &rarr; PutObject (or CopyObject if <code>x-amz-copy-source</code> is set)<br><b>GET</b> &rarr; GetObject (supports Range, conditionals)<br><b>HEAD</b> &rarr; HeadObject<br><b>DELETE</b> &rarr; DeleteObject<br><b>POST ?delete</b> &rarr; DeleteObjects (batch, up to 1000 keys)<br><b>POST ?uploads</b> &rarr; Multipart operations</p>'
    },
    SZ: {
      title: 'Size Check',
      badge: 'decision', badgeText: 'validation',
      body: '<p>Validates the upload before body transmission:</p><p>1. <code>Content-Length</code> header must be present (411 if missing)<br>2. Value must not exceed <code>max_object_size</code> (413 if too large)</p><p>Also wraps the body with <code>http.MaxBytesReader</code> as a safety net against clients that lie about Content-Length.</p>'
    },
    R411: {
      title: '411 Length Required',
      badge: 'reject', badgeText: 'rejection',
      body: '<p>Missing <code>Content-Length</code> header on a PUT request. Required to enforce size policies and pre-allocate backend capacity.</p><p>Response: <code>411 Length Required</code> with <code>MissingContentLength</code> error code.</p>'
    },
    R413: {
      title: '413 Entity Too Large',
      badge: 'reject', badgeText: 'rejection',
      body: '<p>Object size exceeds the configured <code>max_object_size</code> limit.</p><p>Response: <code>413 Request Entity Too Large</code> with <code>EntityTooLarge</code> error code.</p>'
    },
    CAP: {
      title: 'Backend Capacity Check',
      badge: 'decision', badgeText: 'early rejection',
      body: '<p><code>CanAcceptWrite(contentLength)</code> &mdash; checks if <b>any</b> backend has enough free quota for this upload.</p><p>Runs <b>before</b> reading the request body. With <code>Expect: 100-Continue</code>, Go\'s net/http delays the 100 Continue until the first <code>Body.Read()</code>, so the client never transmits bytes for a doomed upload.</p><p class="ac-metric">Metric: s3o_early_rejections_total</p>'
    },
    R507: {
      title: '507 Insufficient Storage',
      badge: 'reject', badgeText: 'rejection',
      body: '<p>No backend has enough free quota for this upload. All backends are either at capacity or their circuit breakers are open.</p><p>Response: <code>507 Insufficient Storage</code> with <code>InsufficientStorage</code> error code.</p>'
    },
    WRITE: {
      title: 'PutObject',
      badge: 'write', badgeText: 'write operation',
      body: '<p>Streams the request body to the selected backend. Backend chosen by routing strategy (<code>spread</code>, <code>fill</code>, or <code>manual</code>).</p><p>Records object metadata (key, size, ETag, backend, content-type) in PostgreSQL. User metadata (<code>x-amz-meta-*</code>) stored as JSONB.</p><p>On backend failure, the failed partial write is enqueued to the <code>cleanup_queue</code> for async retry with exponential backoff.</p>'
    },
    READ: {
      title: 'GetObject / HeadObject',
      badge: 'read', badgeText: 'read operation',
      body: '<p>Retrieves the object from the backend that holds it (looked up in PostgreSQL metadata).</p><p>Supports <code>Range</code> requests (206 Partial Content with <code>Content-Range</code>), conditional requests (<code>If-None-Match</code> &rarr; 304, <code>If-Match</code> &rarr; 412).</p><p>Streams body directly to client via a shared buffer pool &mdash; no full-object buffering in memory. On read failure, automatically fails over to other backends that hold a replica.</p>'
    },
    DEL: {
      title: 'DeleteObject',
      badge: 'write', badgeText: 'write operation',
      body: '<p>Idempotent delete per S3 spec &mdash; deleting a non-existent key succeeds silently.</p><p>Removes the object from <b>all</b> backends that hold a copy, then deletes the metadata row from PostgreSQL.</p><p>Failed backend deletions are enqueued to the <code>cleanup_queue</code> with exponential backoff (1m to 24h, max 10 attempts).</p>'
    },
    CBW: {
      title: 'Circuit Breaker (Write)',
      badge: 'cb', badgeText: 'circuit breaker',
      body: '<p>Three-state circuit breaker wrapping each backend: <b>closed</b> (healthy) &rarr; <b>open</b> (after N consecutive failures) &rarr; <b>half-open</b> (probe after timeout) &rarr; <b>closed</b>.</p><p>When open, returns <code>ErrBackendUnavailable</code> immediately without hitting the backend. Probe-eligible backends (open + timeout elapsed) are allowed through <code>excludeUnhealthy()</code> to prevent deadlock when all backends trip simultaneously.</p><p>Config: <code>circuit_breaker.failure_threshold</code>, <code>circuit_breaker.open_timeout</code>.</p><p class="ac-metric">Metric: s3o_circuit_breaker_state, s3o_circuit_breaker_transitions_total</p><p><a href="../circuit-breaker/">Circuit breaker state machine diagram &rarr;</a></p>'
    },
    CBR: {
      title: 'Circuit Breaker (Read)',
      badge: 'cb', badgeText: 'circuit breaker',
      body: '<p>Same circuit breaker logic as writes, but reads have <b>automatic failover</b>: if the primary backend\'s circuit is open, the request is routed to another backend holding a replica.</p><p>The self-loop "failover" edge represents trying the next available backend until one succeeds or all are exhausted.</p><p><a href="../circuit-breaker/">Circuit breaker state machine diagram &rarr;</a></p>'
    },
    CBD: {
      title: 'Circuit Breaker (Delete)',
      badge: 'cb', badgeText: 'circuit breaker',
      body: '<p>Same circuit breaker logic as writes. If the backend is down, the delete fails with 502 and the orphaned object is enqueued to the <code>cleanup_queue</code> for later retry.</p><p><a href="../circuit-breaker/">Circuit breaker state machine diagram &rarr;</a></p>'
    },
    R502W: {
      title: '502 Bad Gateway (Write)',
      badge: 'reject', badgeText: 'rejection',
      body: '<p>Backend unavailable: circuit breaker is open and not yet probe-eligible (open timeout hasn\'t elapsed).</p><p>The client should retry with a different backend or wait for the circuit to transition to half-open.</p>'
    },
    R502D: {
      title: '502 Bad Gateway (Delete)',
      badge: 'reject', badgeText: 'rejection',
      body: '<p>Backend unavailable for delete. The orphaned object data will be cleaned up asynchronously by the <code>cleanup_queue</code> worker.</p>'
    },
    OK_W: {
      title: '200 OK (Write)',
      badge: 'success', badgeText: 'success',
      body: '<p>Object stored successfully. Response includes the <code>ETag</code> header (MD5 of the uploaded content).</p><p>Metadata recorded in PostgreSQL: key, size, ETag, backend name, content-type, user metadata, timestamps.</p>'
    },
    OK_R: {
      title: '200 / 206 (Read)',
      badge: 'success', badgeText: 'success',
      body: '<p><b>200</b> for full object retrieval. <b>206 Partial Content</b> for Range requests, with <code>Content-Range</code> header.</p><p>Includes <code>ETag</code>, <code>Last-Modified</code>, <code>Content-Type</code>, <code>Accept-Ranges: bytes</code>, and any <code>x-amz-meta-*</code> user metadata.</p>'
    },
    OK_D: {
      title: '204 No Content (Delete)',
      badge: 'success', badgeText: 'success',
      body: '<p>Object deleted successfully (or didn\'t exist &mdash; S3 idempotent delete). No response body.</p>'
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
    if (tooltip.querySelector('a')) pinned = true;
  }

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
        hoveringNode = true; clearTimeout(hideTimer);
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
| <span style="color:#2a9d73">**Green border**</span> | Decision / routing |
| <span style="color:#4aaa8a">**Teal fill**</span> | Concurrency pool (semaphore) |
| <span style="color:#8a9aa8">**Gray**</span> | Processing step |
| <span style="color:#d4a0a0">**Red**</span> | Rejection (4xx/5xx) |
| <span style="color:#34b882">**Green**</span> | Success / read operation |
| <span style="color:#5ec9a0">**Teal**</span> | Write operation |
| <span style="color:#c4a35a">**Amber**</span> | Circuit breaker |
