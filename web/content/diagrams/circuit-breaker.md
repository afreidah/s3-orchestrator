---
title: "Circuit Breaker"
linkTitle: "Circuit Breaker"
weight: 4
---

Three-state circuit breaker state machine shared by the database wrapper (`CircuitBreakerStore`) and per-backend wrapper (`CircuitBreakerBackend`). **Hover over any component** for implementation details.

<style>
  #ac-diagram { margin: 1rem 0; }
  #ac-tooltip {
    position: fixed; z-index: 9999; pointer-events: none;
    max-width: 380px; padding: 0.7rem 0.85rem;
    background: #161b22; border: 1px solid #30363d; border-radius: 6px;
    box-shadow: 0 4px 16px rgba(0,0,0,0.4); display: none;
  }
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
  .ac-badge-cleanup { background: #8b949e22; color: #8b949e; border: 1px solid #8b949e55; }
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
    '    CALL([CBCall /\\nCBCallNoResult]):::entry --> PRE{PreCheck}:::decision',
    '',
    '    PRE -->|state = closed| EXEC[Execute\\nOperation]:::process',
    '',
    '    PRE -->|state = open| TIMEOUT{Open Timeout\\nElapsed?}:::decision',
    '    TIMEOUT -->|no| SENTINEL[Return Sentinel\\nError]:::reject',
    '    TIMEOUT -->|yes| PROBE{Probe Slot\\nAvailable?}:::decision',
    '    PROBE -->|CAS fails| SENTINEL',
    '    PROBE -->|CAS ok| HALFOPEN[Transition\\nto Half-Open]:::process',
    '    HALFOPEN --> EXEC',
    '',
    '    PRE -->|state = half-open| SENTINEL',
    '',
    '    EXEC --> POST{PostCheck\\nError Filter}:::decision',
    '    POST -->|not a circuit error| SUCCESS[Return\\nResult]:::success',
    '',
    '    POST -->|circuit error| FAIL[Increment\\nFailures]:::filter',
    '    FAIL --> WASHO{Was\\nHalf-Open?}:::decision',
    '    WASHO -->|yes| REOPEN[Transition\\nto Open]:::reject',
    '    WASHO -->|no| THRESH{Failures\\n>= Threshold?}:::decision',
    '    THRESH -->|no| PASSTHRU[Return\\nOriginal Error]:::process',
    '    THRESH -->|yes| TRIP[Transition\\nto Open]:::reject',
    '',
    '    POST -->|success + half-open| RECOVER[Transition\\nto Closed]:::success',
    '    RECOVER --> RESET[Reset Failure\\nCounter]:::process',
    '    RESET --> SUCCESS',
    '',
    '    classDef entry fill:#1f6feb,stroke:#1f6feb,color:#fff,font-weight:bold',
    '    classDef filter fill:#9e6a03,stroke:#d29922,color:#fff',
    '    classDef decision fill:#21262d,stroke:#58a6ff,color:#e6edf3,font-size:11px',
    '    classDef process fill:#8957e5,stroke:#bc8cff,color:#fff',
    '    classDef storage fill:#0d419d,stroke:#58a6ff,color:#c9d1d9',
    '    classDef success fill:#238636,stroke:#3fb950,color:#fff,font-weight:bold',
    '    classDef reject fill:#da3633,stroke:#f85149,color:#fff,font-weight:bold',
    '    classDef cleanup fill:#21262d,stroke:#8b949e,color:#e6edf3'
  ].join('\n');

  mermaid.initialize({
    startOnLoad: false, theme: 'dark',
    flowchart: { nodeSpacing: 14, rankSpacing: 22, curve: 'basis', padding: 5, diagramPadding: 8, useMaxWidth: true }
  });

  mermaid.render('cb-mermaid-svg', diagramSrc).then(function(result) {
    document.getElementById('ac-diagram').innerHTML = result.svg;
    wireUpInteractivity();
  });

  var nodeInfo = {
    CALL: {
      title: 'CBCall / CBCallNoResult',
      badge: 'entry', badgeText: 'entry point',
      body: '<p>Generic call wrappers that guard any operation with circuit breaker logic. <code>CBCall[T]</code> handles functions returning <code>(T, error)</code>, while <code>CBCallNoResult</code> handles <code>error</code>-only functions.</p><p>Used by <code>CircuitBreakerBackend</code> (wraps all S3 operations: PutObject, GetObject, HeadObject, DeleteObject) and <code>CircuitBreakerStore</code> (wraps all PostgreSQL metadata operations).</p><p>Flow: PreCheck &rarr; execute &rarr; PostCheck. If PreCheck returns an error, the real operation is never called.</p>'
    },
    PRE: {
      title: 'PreCheck',
      badge: 'decision', badgeText: 'state machine gate',
      body: '<p><code>cb.PreCheck()</code> is the entry gate that inspects the current circuit state under a mutex lock.</p><p><b>Closed</b>: returns <code>nil</code> &mdash; all calls pass through to the real operation.<br><b>Open</b>: checks if <code>openTimeout</code> has elapsed since <code>lastFailure</code>. If not, returns the sentinel error immediately (no I/O).<br><b>Half-Open</b>: returns sentinel &mdash; only the single probe request is allowed through.</p><p>This is the fast-rejection path: open circuits never touch the real backend or database.</p>'
    },
    EXEC: {
      title: 'Execute Operation',
      badge: 'process', badgeText: 'real call',
      body: '<p>Calls the wrapped function &mdash; the actual S3 backend call or database query. This only runs when PreCheck returns <code>nil</code> (circuit closed, or probe allowed through).</p><p>For backends: <code>cb.real.PutObject()</code>, <code>cb.real.GetObject()</code>, etc.<br>For database: <code>cb.real.GetObjectLocation()</code>, <code>cb.real.RecordObject()</code>, etc.</p><p>The result and error are passed to PostCheck for state machine evaluation.</p>'
    },
    TIMEOUT: {
      title: 'Open Timeout Elapsed?',
      badge: 'decision', badgeText: 'recovery timer',
      body: '<p>Checks <code>time.Since(cb.lastFailure) >= cb.openTimeout</code>.</p><p>Configurable per circuit breaker type:<br><b>Database</b>: <code>circuit_breaker.open_timeout</code> (default: 15s)<br><b>Backend</b>: <code>backend_circuit_breaker.open_timeout</code> (default: 5m)<br><b>Redis</b>: <code>counters.redis.open_timeout</code> (default: 15s)</p><p>Until the timeout elapses, all requests receive the sentinel error without any I/O. This protects a failing backend from being hammered with retries.</p>'
    },
    SENTINEL: {
      title: 'Return Sentinel Error',
      badge: 'reject', badgeText: 'fast rejection',
      body: '<p>Returns the circuit-specific sentinel error immediately, with no I/O to the underlying system.</p><p><b>Backend</b>: <code>ErrBackendUnavailable</code> &mdash; triggers write failover to the next eligible backend, or broadcast-read fallback.<br><b>Database</b>: <code>ErrDBUnavailable</code> &mdash; triggers degraded mode: writes return 503, reads fan out to all backends.<br><b>Redis</b>: <code>ErrDBUnavailable</code> &mdash; falls back to local in-memory counters.</p><p>The manager\'s <code>excludeUnhealthy()</code> filter uses <code>ProbeEligible()</code> to pre-screen backends before routing, so most open-circuit rejections happen at the routing layer rather than here.</p>'
    },
    PROBE: {
      title: 'Probe Slot Available?',
      badge: 'decision', badgeText: 'atomic CAS',
      body: '<p><code>cb.probeInFlight.CompareAndSwap(false, true)</code> &mdash; atomic compare-and-swap ensures exactly <b>one</b> probe request passes through at a time.</p><p>If a probe is already in flight (another goroutine won the CAS), this request gets the sentinel error. This prevents a thundering herd of probe requests when multiple goroutines detect the timeout simultaneously.</p><p>The <code>probeInFlight</code> flag is cleared in both <code>onSuccess()</code> (probe succeeded, circuit closes) and <code>onFailure()</code> (probe failed, circuit re-opens).</p>'
    },
    HALFOPEN: {
      title: 'Transition to Half-Open',
      badge: 'process', badgeText: 'state transition',
      body: '<p><code>cb.transition(stateHalfOpen)</code> &mdash; allows exactly one probe request through to test whether the backend or database has recovered.</p><p>Logs: <code>"Circuit breaker half-open: probing"</code> with <code>open_duration</code> showing how long the circuit was open.</p><p class="ac-metric">Metric: s3o_circuit_breaker_transitions_total{name, from="open", to="half-open"}<br>Gauge: s3o_circuit_breaker_state{name} = 2</p>'
    },
    POST: {
      title: 'PostCheck / Error Filter',
      badge: 'decision', badgeText: 'result evaluation',
      body: '<p><code>cb.PostCheck(err)</code> passes the operation result through the pluggable error filter (<code>cb.isError(err)</code>) to decide whether it counts as a circuit-breaker failure.</p><p><b>Backend filter</b> (<code>isBackendError</code>): all non-nil errors count &mdash; any backend error is a genuine failure (network, auth, etc.).</p><p><b>Database filter</b> (<code>isDBError</code>): exempts application-level errors (<code>S3Error</code>, <code>ErrNoSpaceAvailable</code>) that indicate the DB is reachable but the request is invalid. Only infrastructure errors (connection refused, timeout) trip the breaker.</p><p>If the error passes the filter, flows to failure handling. If not (or nil), flows to success.</p>'
    },
    SUCCESS: {
      title: 'Return Result',
      badge: 'success', badgeText: 'success',
      body: '<p>The operation succeeded (or the error was not a circuit-breaker error). Returns the result to the caller.</p><p>If the circuit was closed, the failure counter was reset to 0 by <code>onSuccess()</code>. The circuit remains closed and all subsequent requests continue passing through normally.</p>'
    },
    FAIL: {
      title: 'Increment Failures',
      badge: 'filter', badgeText: 'failure tracking',
      body: '<p><code>cb.onFailure()</code> increments <code>cb.failures++</code> and records <code>cb.lastFailure = time.Now()</code>.</p><p>The failure counter tracks <b>consecutive</b> failures. It is reset to 0 on any successful call via <code>onSuccess()</code>. This means intermittent errors (occasional timeouts in otherwise healthy traffic) do not trip the breaker.</p><p>The <code>lastFailure</code> timestamp is used to calculate when the <code>openTimeout</code> elapses for probe eligibility.</p>'
    },
    WASHO: {
      title: 'Was Half-Open?',
      badge: 'decision', badgeText: 'probe result',
      body: '<p>If the circuit was in <code>stateHalfOpen</code> when the failure occurred, the probe request failed &mdash; the backend or database is still down.</p><p>The <code>probeInFlight</code> atomic flag is cleared (<code>Store(false)</code>) so a future probe can be attempted after the open timeout elapses again.</p><p>The circuit transitions directly back to open without waiting for the failure threshold.</p>'
    },
    REOPEN: {
      title: 'Transition to Open (probe failed)',
      badge: 'reject', badgeText: 'state transition',
      body: '<p><code>cb.transition(stateOpen)</code> &mdash; probe failed, circuit re-opens. The <code>lastFailure</code> timestamp resets the open timeout window.</p><p>Logs: <code>"Circuit breaker reopened: probe failed"</code> with current failure count.</p><p>The cycle repeats: after <code>openTimeout</code> elapses, another single probe will be attempted.</p><p class="ac-metric">Metric: s3o_circuit_breaker_transitions_total{name, from="half-open", to="open"}<br>Gauge: s3o_circuit_breaker_state{name} = 1</p>'
    },
    THRESH: {
      title: 'Failures >= Threshold?',
      badge: 'decision', badgeText: 'threshold check',
      body: '<p>Compares <code>cb.failures >= cb.failThreshold</code>.</p><p>Configurable thresholds:<br><b>Database</b>: <code>circuit_breaker.failure_threshold</code> (default: 3)<br><b>Backend</b>: <code>backend_circuit_breaker.failure_threshold</code> (default: 5)<br><b>Redis</b>: <code>counters.redis.failure_threshold</code> (default: 3)</p><p>Below threshold, the error is returned to the caller as-is (not replaced with the sentinel), allowing the caller to handle it normally while the breaker continues tracking.</p>'
    },
    PASSTHRU: {
      title: 'Return Original Error',
      badge: 'process', badgeText: 'below threshold',
      body: '<p>Failure count is below the threshold. The original error is returned to the caller unchanged. The circuit remains closed.</p><p>The caller handles the error normally &mdash; for backends, this may trigger write failover to another backend. For database operations, the error propagates to the HTTP handler.</p><p>The failure counter persists: if the next call also fails, it increments further toward the threshold. A single success resets the counter to 0.</p>'
    },
    TRIP: {
      title: 'Transition to Open (threshold reached)',
      badge: 'reject', badgeText: 'state transition',
      body: '<p><code>cb.transition(stateOpen)</code> &mdash; consecutive failure threshold reached. The circuit opens, and <code>cb.openedAt</code> is recorded for duration tracking.</p><p>Logs: <code>"Circuit breaker opened: failure threshold reached"</code> with failure count and threshold.</p><p>The original error is replaced with the sentinel error (<code>ErrBackendUnavailable</code> or <code>ErrDBUnavailable</code>) so callers always see the canonical error type.</p><p class="ac-metric">Metric: s3o_circuit_breaker_transitions_total{name, from="closed", to="open"}<br>Gauge: s3o_circuit_breaker_state{name} = 1</p>'
    },
    RECOVER: {
      title: 'Transition to Closed (recovered)',
      badge: 'success', badgeText: 'state transition',
      body: '<p><code>cb.transition(stateClosed)</code> &mdash; probe succeeded, the backend or database is healthy again.</p><p>Logs: <code>"Circuit breaker closed: recovered"</code> with <code>degraded_duration</code> showing total time spent in open + half-open states.</p><p>The <code>probeInFlight</code> flag is cleared and all subsequent requests pass through normally.</p><p class="ac-metric">Metric: s3o_circuit_breaker_transitions_total{name, from="half-open", to="closed"}<br>Gauge: s3o_circuit_breaker_state{name} = 0</p>'
    },
    RESET: {
      title: 'Reset Failure Counter',
      badge: 'process', badgeText: 'counter reset',
      body: '<p><code>cb.failures = 0</code> &mdash; clears the consecutive failure counter on any successful operation.</p><p>This happens both during normal closed-state operation (preventing spurious trips from intermittent errors) and after a successful probe (part of the recovery path).</p><p>The counter is also implicitly reset when the circuit transitions to closed, since the next failure sequence starts fresh.</p>'
    }
  };

  var tooltip = document.getElementById('ac-tooltip');
  var mouseX = 0, mouseY = 0;
  document.addEventListener('mousemove', function(e) {
    mouseX = e.clientX; mouseY = e.clientY;
    if (tooltip.style.display === 'block') positionTooltip();
  });
  function positionTooltip() {
    var pad = 12, x = mouseX + pad, y = mouseY + pad;
    if (x + tooltip.offsetWidth > window.innerWidth - pad) x = mouseX - tooltip.offsetWidth - pad;
    if (y + tooltip.offsetHeight > window.innerHeight - pad) y = mouseY - tooltip.offsetHeight - pad;
    tooltip.style.left = x + 'px'; tooltip.style.top = y + 'px';
  }
  function showInfo(id) {
    var info = nodeInfo[id];
    if (!info) { tooltip.style.display = 'none'; return; }
    tooltip.innerHTML = '<h3>' + info.title + '</h3><span class="ac-badge ac-badge-' + info.badge + '">' + info.badgeText + '</span>' + info.body;
    tooltip.style.display = 'block'; positionTooltip();
  }
  function clearInfo() { tooltip.style.display = 'none'; }

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
        svg.classList.remove('highlighting');
        svg.querySelectorAll('.highlight').forEach(function(el) { el.classList.remove('highlight'); });
        clearInfo();
      });
    });
  }
})();
</script>

## Legend

| Color | Meaning |
|-------|---------|
| <span style="color:#1f6feb">**Blue**</span> | Entry point |
| <span style="color:#d29922">**Amber**</span> | Failure tracking |
| <span style="color:#58a6ff">**Blue border**</span> | Decision / branch |
| <span style="color:#bc8cff">**Purple**</span> | Processing step |
| <span style="color:#3fb950">**Green**</span> | Success / recovery |
| <span style="color:#f85149">**Red**</span> | Rejection / circuit open |

