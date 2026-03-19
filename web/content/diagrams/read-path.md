---
title: "Read Path"
linkTitle: "Read Path"
weight: 3
---

Detailed flow of a GetObject request through location lookup, failover, broadcast reads, decryption, and streaming. **Hover over any component** for implementation details.

<style>
  #ac-diagram { margin: 1rem 0; }
  #ac-tooltip {
    position: fixed; z-index: 9999;
    max-width: 380px; padding: 0.7rem 0.85rem;
    background: #161b22; border: 1px solid #30363d; border-radius: 6px;
    box-shadow: 0 4px 16px rgba(0,0,0,0.4); display: none;
  }
  #ac-tooltip a { color: #58a6ff; text-decoration: none; }
  #ac-tooltip a:hover { text-decoration: underline; }
  #ac-tooltip h3 { color: #58a6ff; font-size: 0.85rem; margin: 0 0 0.25rem 0; }
  #ac-tooltip .ac-badge {
    display: inline-block; padding: 1px 7px; border-radius: 4px;
    font-size: 0.6rem; font-weight: 600; margin-bottom: 0.4rem; text-transform: uppercase;
  }
  .ac-badge-entry { background: #1f6feb22; color: #58a6ff; border: 1px solid #58a6ff55; }
  .ac-badge-filter { background: #9e6a0322; color: #d29922; border: 1px solid #d2992255; }
  .ac-badge-decision { background: #58a6ff22; color: #58a6ff; border: 1px solid #58a6ff55; }
  .ac-badge-process { background: #8957e522; color: #bc8cff; border: 1px solid #bc8cff55; }
  .ac-badge-storage { background: #0d419d22; color: #79c0ff; border: 1px solid #79c0ff55; }
  .ac-badge-success { background: #23863622; color: #3fb950; border: 1px solid #3fb95055; }
  .ac-badge-reject { background: #da363322; color: #f85149; border: 1px solid #f8514955; }
  #ac-tooltip p { font-size: 0.75rem; line-height: 1.4; color: #c9d1d9; margin-bottom: 0.35rem; }
  #ac-tooltip code { background: #21262d; padding: 1px 4px; border-radius: 3px; font-size: 0.7rem; color: #79c0ff; }
  #ac-tooltip .ac-metric { color: #d2a8ff; font-style: italic; font-size: 0.7rem; }
  #ac-diagram .node, #ac-diagram .edgePath, #ac-diagram .edgeLabel { transition: opacity 0.15s, filter 0.15s; }
  #ac-diagram svg.highlighting .node, #ac-diagram svg.highlighting .edgePath, #ac-diagram svg.highlighting .edgeLabel { opacity: 0.12; }
  #ac-diagram svg.highlighting .node.highlight, #ac-diagram svg.highlighting .edgePath.highlight, #ac-diagram svg.highlighting .edgeLabel.highlight { opacity: 1; filter: drop-shadow(0 0 6px rgba(88,166,255,0.5)); }
  #ac-diagram .node { cursor: pointer; }
</style>

<div id="ac-diagram"></div>
<div id="ac-tooltip"></div>

<script src="https://cdn.jsdelivr.net/npm/mermaid@11/dist/mermaid.min.js"></script>
<script>
(function() {
  var diagramSrc = [
    'flowchart TD',
    '    GET([GetObject\\nRequest]):::entry --> DBLOOKUP[DB Lookup:\\nGetAllObjectLocations]:::storage',
    '',
    '    DBLOOKUP -->|not found| R404[404 Not\\nFound]:::reject',
    '    DBLOOKUP -->|DB unavailable| CACHE{Location\\nCache?}:::decision',
    '    DBLOOKUP -->|ok| COPIES[Iterate Copies\\nwith Failover]:::process',
    '',
    '    CACHE -->|hit + success| STREAM',
    '    CACHE -->|miss or fail| FANOUT[Broadcast to\\nAll Backends]:::process',
    '    FANOUT -->|first success| STREAM',
    '    FANOUT -->|all fail| BFAIL[Return Last\\nError]:::reject',
    '',
    '    COPIES --> ULIMIT{Usage Limit\\nCheck}:::filter',
    '    ULIMIT -->|over limit, more copies| COPIES',
    '    ULIMIT -->|all over limit| RLIMIT[429 Usage\\nLimit Exceeded]:::reject',
    '    ULIMIT -->|within limits| FETCH[Backend\\nGetObject]:::process',
    '',
    '    FETCH --> CB{Circuit\\nBreaker}:::decision',
    '    CB -->|open| FETCHFAIL',
    '    CB -->|closed/probe| S3[S3 Backend\\nGetObject]:::storage',
    '    S3 -->|error| FETCHFAIL{Fetch\\nFailed?}:::decision',
    '    S3 -->|success| EGRESSCHK',
    '',
    '    FETCHFAIL -->|copies remain| COPIES',
    '    FETCHFAIL -->|all exhausted| RETERR[Return Last\\nError]:::reject',
    '',
    '    EGRESSCHK{Egress Limit\\nCheck}:::filter',
    '    EGRESSCHK -->|over limit| COPIES',
    '    EGRESSCHK -->|within limits| DECRYPT{Decrypt\\nNeeded?}:::decision',
    '',
    '    DECRYPT -->|encrypted + range| DECRANGE[DecryptRange:\\nChunk Slice]:::process',
    '    DECRYPT -->|encrypted, full| DECFULL[Decrypt:\\nFull Stream]:::process',
    '    DECRYPT -->|plaintext| STREAM',
    '    DECRANGE --> STREAM',
    '    DECFULL --> STREAM',
    '',
    '    STREAM[Stream Body\\nto Client]:::process --> METRICS[Record Usage\\n& Metrics]:::process',
    '    METRICS --> OK[Return\\nGetObjectResult]:::success',
    '',
    '    classDef entry fill:#1f6feb,stroke:#1f6feb,color:#fff,font-weight:bold',
    '    classDef filter fill:#9e6a03,stroke:#d29922,color:#fff',
    '    classDef decision fill:#21262d,stroke:#58a6ff,color:#e6edf3,font-size:11px',
    '    classDef process fill:#8957e5,stroke:#bc8cff,color:#fff',
    '    classDef storage fill:#0d419d,stroke:#58a6ff,color:#c9d1d9',
    '    classDef success fill:#238636,stroke:#3fb950,color:#fff,font-weight:bold',
    '    classDef reject fill:#da3633,stroke:#f85149,color:#fff,font-weight:bold'
  ].join('\n');

  mermaid.initialize({
    startOnLoad: false, theme: 'dark',
    flowchart: { nodeSpacing: 14, rankSpacing: 22, curve: 'basis', padding: 5, diagramPadding: 8, useMaxWidth: true }
  });

  mermaid.render('read-mermaid-svg', diagramSrc).then(function(result) {
    document.getElementById('ac-diagram').innerHTML = result.svg;
    wireUpInteractivity();
  });

  var nodeInfo = {
    GET: {
      title: 'GetObject Request',
      badge: 'entry', badgeText: 'entry point',
      body: '<p>Incoming GET request after passing through admission control, rate limiting, and SigV4 authentication.</p><p>The HTTP handler extracts the object key and optional <code>Range</code> header. Conditional request headers (<code>If-None-Match</code>, <code>If-Modified-Since</code>) are handled at the HTTP layer before reaching the storage manager.</p>'
    },
    DBLOOKUP: {
      title: 'DB Lookup: GetAllObjectLocations',
      badge: 'storage', badgeText: 'PostgreSQL',
      body: '<p><code>store.GetAllObjectLocations(ctx, key)</code> queries PostgreSQL for all copies of the object across backends.</p><p>Returns a slice of <code>ObjectLocation</code> structs containing: backend name, size, encryption metadata (encrypted flag, wrapped DEK, key ID, plaintext size), and timestamps.</p><p>The query goes through a circuit breaker &mdash; if the DB circuit breaker is open, returns <code>ErrDBUnavailable</code> which triggers the broadcast read path.</p>'
    },
    R404: {
      title: '404 Not Found',
      badge: 'reject', badgeText: 'not found',
      body: '<p>No record for this object key exists in the database. The HTTP handler translates <code>ErrObjectNotFound</code> to a standard S3 <code>NoSuchKey</code> XML error response with HTTP 404.</p>'
    },
    CACHE: {
      title: 'Location Cache Lookup',
      badge: 'decision', badgeText: 'degraded mode',
      body: '<p>When the DB is unavailable, the system enters degraded mode and checks the in-memory location cache first.</p><p>The cache maps object keys to backend names with a TTL (+/-20% random jitter to prevent expiry storms). Populated by previous successful broadcast reads.</p><p>On <b>cache hit</b>: tries the cached backend directly. If that succeeds, the read completes without broadcasting. If the cached backend fails, falls through to broadcast.</p><p>On <b>cache miss</b>: proceeds directly to broadcast.</p><p class="ac-metric">Metrics: s3o_degraded_reads_total, s3o_degraded_cache_hits_total</p>'
    },
    FANOUT: {
      title: 'Broadcast to All Backends',
      badge: 'process', badgeText: 'broadcast',
      body: '<p>Tries every configured backend to find the object without DB guidance. Two strategies (configured via <code>parallel_broadcast</code>):</p><p><b>Sequential</b>: iterates backends in order, stops at first success. Lower resource usage but higher latency.</p><p><b>Parallel</b>: launches a goroutine per backend, returns the first success via buffered channel. Losing goroutines\' bodies are closed via <code>sync.Once</code>. Lower latency but more API calls.</p><p>On success, caches the backend mapping (<code>cache.Set(key, name)</code>) so future degraded reads skip the broadcast.</p><p class="ac-metric">Span attribute: s3o.parallel_broadcast=true</p>'
    },
    BFAIL: {
      title: 'Return Last Error (Broadcast)',
      badge: 'reject', badgeText: 'all backends failed',
      body: '<p>All backends failed during broadcast read. Returns the last error wrapped with "all backends failed during degraded read".</p><p>The HTTP handler distinguishes <code>ErrObjectNotFound</code> (404) from backend errors (502) based on the error type.</p>'
    },
    COPIES: {
      title: 'Iterate Copies with Failover',
      badge: 'process', badgeText: 'failover loop',
      body: '<p><code>withReadFailover()</code> iterates through all copies returned by the DB lookup. Each copy is tried in order (primary first, then replicas).</p><p>On failure, logs a warning and moves to the next copy. If all copies fail, returns the last error. If all copies were skipped due to usage limits, returns <code>ErrUsageLimitExceeded</code> specifically.</p><p class="ac-metric">Span attribute: s3o.failover=true (when primary fails)</p>'
    },
    ULIMIT: {
      title: 'Usage Limit Check (Pre-fetch)',
      badge: 'filter', badgeText: 'quota check',
      body: '<p><code>usage.WithinLimits(backendName, apiCalls=1, egress=0, ingress=0)</code></p><p>Checks if this backend can accept one more API call without exceeding its monthly limits. This is a <b>pre-fetch</b> check &mdash; egress is checked again after the response size is known.</p><p>Skipping over-limit backends avoids wasting API calls on backends that can\'t serve the egress anyway. If copies remain on other backends, the loop continues. If all copies were on over-limit backends, returns <code>ErrUsageLimitExceeded</code>.</p><p class="ac-metric">Metric: s3o_usage_limit_rejections_total{operation="GetObject", direction="read"}</p>'
    },
    RLIMIT: {
      title: '429 Usage Limit Exceeded',
      badge: 'reject', badgeText: 'rate limited',
      body: '<p>All copies of this object are on backends that have exceeded their monthly usage limits (API calls or egress).</p><p>The HTTP handler translates <code>ErrUsageLimitExceeded</code> to a 429 Too Many Requests response with a <code>SlowDown</code> S3 error code.</p>'
    },
    FETCH: {
      title: 'Backend GetObject',
      badge: 'process', badgeText: 'fetch',
      body: '<p>Calls <code>backend.GetObject(ctx, key, actualRange)</code> through the circuit breaker wrapper.</p><p>For <b>encrypted objects with a Range header</b>, the plaintext range is first translated to ciphertext chunk offsets via <code>encryption.CiphertextRange()</code>. Whole ciphertext chunks are fetched (each has its own GCM auth tag), and only the requested plaintext bytes are returned after decryption.</p><p>The timeout context (<code>o.withTimeout(ctx)</code>) applies a per-backend deadline from <code>backend_timeout</code> config.</p>'
    },
    CB: {
      title: 'Circuit Breaker',
      badge: 'decision', badgeText: 'circuit breaker',
      body: '<p><code>CircuitBreakerBackend.GetObject()</code> wraps the real S3 call with <code>CBCall()</code>:</p><p><b>PreCheck</b>: if circuit is open and not probe-eligible, return <code>ErrBackendUnavailable</code> immediately.<br><b>On success</b>: if half-open, transition to closed (recovered).<br><b>On failure</b>: increment failure counter; if threshold reached, open the circuit.</p><p><a href="../circuit-breaker/">Circuit breaker state machine diagram &rarr;</a></p>'
    },
    S3: {
      title: 'S3 Backend GetObject',
      badge: 'storage', badgeText: 'S3 API call',
      body: '<p>AWS SDK v2 <code>s3.GetObject()</code> call to the backend endpoint. Returns <code>GetObjectResult</code> with Body (io.ReadCloser), Size, ContentType, ETag, LastModified, and Metadata.</p><p>For range requests, the backend returns HTTP 206 Partial Content with only the requested byte range.</p><p class="ac-metric">Metrics: s3o_backend_requests_total, s3o_backend_latency_seconds</p>'
    },
    FETCHFAIL: {
      title: 'Fetch Failed?',
      badge: 'decision', badgeText: 'failover',
      body: '<p>On backend error (network timeout, S3 error, circuit breaker rejection), the failed copy is skipped. Usage is still recorded for the API call attempt.</p><p>If copies remain, the failover loop continues with the next replica. If all copies are exhausted, returns the last error (502 Bad Gateway).</p>'
    },
    RETERR: {
      title: 'Return Last Error',
      badge: 'reject', badgeText: 'failure',
      body: '<p>All copies have been tried and failed. Returns the last error encountered, which propagates to the HTTP handler as a 502 Bad Gateway.</p><p>The span is marked with error status and the error is recorded for tracing.</p>'
    },
    EGRESSCHK: {
      title: 'Egress Limit Check (Post-fetch)',
      badge: 'filter', badgeText: 'egress check',
      body: '<p><code>usage.WithinLimits(backendName, 1, response.Size, 0)</code></p><p>Now that the response size is known, checks if downloading this object would exceed the backend\'s monthly egress limit.</p><p>If over limit: closes the response body (releasing the HTTP connection), records the API call against usage, and loops back to try the next copy. If no copies remain within limits, returns <code>ErrUsageLimitExceeded</code>.</p>'
    },
    DECRYPT: {
      title: 'Decrypt Needed?',
      badge: 'decision', badgeText: 'decryption check',
      body: '<p>Checks if this copy\'s DB record has <code>Encrypted: true</code> and the encryptor is configured. Three paths:</p><p>1. <b>Encrypted + range request</b>: use <code>DecryptRange()</code> on fetched ciphertext chunks<br>2. <b>Encrypted, full read</b>: use <code>Decrypt()</code> on the entire ciphertext stream<br>3. <b>Plaintext</b>: pass body through directly</p>'
    },
    DECRANGE: {
      title: 'DecryptRange: Chunk Slice',
      badge: 'process', badgeText: 'range decryption',
      body: '<p>Envelope decryption for range requests:</p><p>1. <code>UnpackKeyData(loc.EncryptionKey)</code> &mdash; extract <code>baseNonce</code> and <code>wrappedDEK</code><br>2. <code>encryptor.DecryptRange(ctx, body, wrappedDEK, keyID, rangeResult, baseNonce)</code></p><p>Decrypts only the fetched ciphertext chunks, then slices to the exact requested plaintext bytes. Sets <code>Content-Range</code> header using the original plaintext offsets.</p><p>Response size is set to the plaintext range length. The body is wrapped with <code>wrapReader()</code> so Close reaches the original HTTP body.</p><p class="ac-metric">Metric: s3o_encryption_ops_total{operation="decrypt_range"}</p>'
    },
    DECFULL: {
      title: 'Decrypt: Full Stream',
      badge: 'process', badgeText: 'full decryption',
      body: '<p>Envelope decryption for full reads:</p><p>1. <code>UnpackKeyData(loc.EncryptionKey)</code> &mdash; extract <code>baseNonce</code> and <code>wrappedDEK</code><br>2. <code>encryptor.Decrypt(ctx, body, wrappedDEK, keyID)</code></p><p>Streams AES-256-GCM decryption chunk by chunk. Each chunk\'s authentication tag is verified independently. Response size is set to <code>loc.PlaintextSize</code> from the DB record.</p><p class="ac-metric">Metric: s3o_encryption_ops_total{operation="decrypt"}</p>'
    },
    STREAM: {
      title: 'Stream Body to Client',
      badge: 'process', badgeText: 'streaming',
      body: '<p>The (possibly decrypted) response body streams directly to the HTTP client. Uses <code>sync.Once</code> to protect the result assignment when parallel broadcast is enabled &mdash; only the first successful response is returned, and losing responses have their bodies closed.</p><p>The body is an <code>io.ReadCloser</code>; the HTTP handler streams it to the response writer and closes it when done.</p>'
    },
    METRICS: {
      title: 'Record Usage & Metrics',
      badge: 'process', badgeText: 'telemetry',
      body: '<p><code>Record(backendName, apiCalls=1, egress=result.Size, ingress=0)</code> increments the monthly usage counters in the counter backend.</p><p>Operation duration is recorded via <code>MetricsCollector</code>. If failover occurred (primary copy failed), the span includes <code>s3o.failover=true</code>.</p><p>Audit event: <code>storage.GetObject</code> with key, backend name, and size.</p>'
    },
    OK: {
      title: 'Return GetObjectResult',
      badge: 'success', badgeText: 'success',
      body: '<p>Returns <code>GetObjectResult</code> containing: Body (io.ReadCloser), Size (plaintext size for encrypted objects), ContentType, ETag, LastModified, Metadata, and ContentRange (for range requests).</p><p>The HTTP handler sets response headers and streams the body to the client. For encrypted objects, the size and ETag reflect plaintext values for S3 client compatibility.</p>'
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
| <span style="color:#1f6feb">**Blue**</span> | Entry point |
| <span style="color:#d29922">**Amber**</span> | Usage limit filtering |
| <span style="color:#58a6ff">**Blue border**</span> | Decision / branch |
| <span style="color:#bc8cff">**Purple**</span> | Processing step |
| <span style="color:#79c0ff">**Dark blue**</span> | Storage / DB / S3 |
| <span style="color:#3fb950">**Green**</span> | Success |
| <span style="color:#f85149">**Red**</span> | Rejection / failure |
