---
description: "Interactive state machine for the circuit breaker that guards the database, each backend, and the Redis counter backend."
title: "Circuit Breaker"
linkTitle: "Circuit Breaker"
weight: 4
---

State machine of `breaker.CircuitBreaker`, the one breaker type used in three places. The database breaker wraps every SQL call in the store driver (SQLite or PostgreSQL) and recovers through half-open probing: after the open timeout, one real query is let through as a probe. Backend breakers (`CircuitBreakerBackend`, enabled with `backend_circuit_breaker.enabled`) and the Redis counter breaker use external recovery instead: while open no call reaches the backend or Redis, and only a separate health check closes them, going straight from open to closed. **Hover over any component** for implementation details.

<style>
  #ac-diagram { margin: 1rem 0; }
  #ac-tooltip {
    position: fixed; z-index: 9999; pointer-events: none;
    max-width: 380px; padding: 0.7rem 0.85rem;
    background: #161b22; border: 1px solid #30363d; border-radius: 6px;
    box-shadow: 0 4px 16px rgba(0,0,0,0.4); display: none;
  }
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

<script src="https://cdn.jsdelivr.net/npm/mermaid@11.8.0/dist/mermaid.min.js"></script>
<script>
(function() {
  var diagramSrc = [
    'flowchart TD',
    '    CALL([cb.Call /<br>cb.CallNoResult]):::entry --> PRE{PreCheck}:::decision',
    '',
    '    PRE -->|state = closed| EXEC[Execute<br>Operation]:::process',
    '',
    '    PRE -->|open, backend breaker| SENTINEL[Return Sentinel<br>Error]:::reject',
    '    PRE -->|open, database breaker| TIMEOUT{Open Timeout<br>Elapsed?<br>database only}:::decision',
    '    TIMEOUT -->|no| SENTINEL',
    '    TIMEOUT -->|yes| PROBE{Probe Slot<br>Available?<br>database only}:::decision',
    '    PROBE -->|CAS fails| SENTINEL',
    '    PROBE -->|CAS ok| HALFOPEN[Transition<br>to Half-Open<br>database only]:::process',
    '    HALFOPEN --> EXEC',
    '',
    '    PRE -->|state = half-open| STALE{Probe Stale?<br>over 2 min<br>database only}:::decision',
    '    STALE -->|no| SENTINEL',
    '    STALE -->|yes / watchdog| REOPEN2[Reset to<br>Open]:::reject',
    '',
    '    EXEC --> POST{PostCheck<br>Error Filter}:::decision',
    '    POST -->|not a circuit error| SUCCESS[Return<br>Result]:::success',
    '',
    '    POST -->|circuit error| FAIL[Increment<br>Failures]:::filter',
    '    FAIL --> WASHO{Was<br>Half-Open?<br>database only}:::decision',
    '    WASHO -->|yes| REOPEN[Transition<br>to Open<br>database only]:::reject',
    '    WASHO -->|no| THRESH{Failures at<br>Threshold?}:::decision',
    '    THRESH -->|no| PASSTHRU[Return<br>Original Error]:::process',
    '    THRESH -->|yes| TRIP[Transition<br>to Open]:::reject',
    '',
    '    POST -->|success + half-open| RECOVER[Transition<br>to Closed<br>database only]:::success',
    '    RECOVER --> RESET[Reset Failure<br>Counter]:::process',
    '    RESET --> SUCCESS',
    '',
    '    WATCHDOG([Backend and Redis<br>Health Check Loop]):::entry --> HEALTH{Health Check<br>Passes?}:::decision',
    '    HEALTH -->|no| BACKOFF[Stay Open,<br>Check Again Later]:::reject',
    '    HEALTH -->|yes| EXTRECOVER[Recover:<br>Open to Closed]:::success',
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

  mermaid.render('cb-mermaid-svg', diagramSrc).then(function(result) {
    document.getElementById('ac-diagram').innerHTML = result.svg;
    wireUpInteractivity();
  });

  var nodeInfo = {
    CALL: {
      title: 'cb.Call / cb.CallNoResult',
      badge: 'entry', badgeText: 'entry point',
      body: '<p>Methods on <code>breaker.CircuitBreaker</code> that guard one operation. <code>cb.Call(fn)</code> handles functions returning <code>(T, error)</code>; <code>cb.CallNoResult(fn)</code> handles <code>error</code>-only functions.</p><p><code>CircuitBreakerBackend</code> guards PutObject, GetObject, HeadObject, DeleteObject and CopyObject this way. The database breaker runs the same PreCheck and PostCheck around every SQL Exec, Query and QueryRow in the store driver (SQLite or PostgreSQL).</p><p>The Redis counter backend never calls PreCheck. It only feeds call results to PostCheck to count failures.</p><p>Flow: PreCheck &rarr; execute &rarr; PostCheck. If PreCheck returns an error, the real operation is never called.</p>'
    },
    PRE: {
      title: 'PreCheck',
      badge: 'decision', badgeText: 'state machine gate',
      body: '<p><code>cb.PreCheck()</code> is the entry gate that inspects the current circuit state under a mutex lock.</p><p><b>Closed</b>: returns <code>nil</code> &mdash; all calls pass through to the real operation.<br><b>Open, backend breaker</b>: always returns the sentinel error. Backend breakers never go half-open.<br><b>Open, database breaker</b>: checks if <code>openTimeout</code> plus jitter has elapsed since <code>lastFailure</code>. If not, returns the sentinel error immediately (no I/O).<br><b>Half-Open</b> (database only): returns sentinel &mdash; only the single probe request is allowed through.</p><p>This is the fast-rejection path: open circuits never touch the real backend or database.</p>'
    },
    EXEC: {
      title: 'Execute Operation',
      badge: 'process', badgeText: 'real call',
      body: '<p>Calls the wrapped function &mdash; the actual S3 backend call or database query. This only runs when PreCheck returns <code>nil</code> (circuit closed, or the database probe allowed through).</p><p>For backends: <code>cb.inner.PutObject()</code>, <code>cb.inner.GetObject()</code>, etc.<br>For database: <code>inner.Exec()</code>, <code>inner.Query()</code>, <code>inner.QueryRow()</code> on the wrapped sqlc DBTX (PostgreSQL), or the matching <code>database/sql</code> calls (SQLite).</p><p>The result and error are passed to PostCheck for state machine evaluation.</p>'
    },
    TIMEOUT: {
      title: 'Open Timeout Elapsed? (database only)',
      badge: 'decision', badgeText: 'recovery timer',
      body: '<p>Checks <code>time.Since(cb.lastFailure) >= cb.openTimeout + cb.probeJitter</code>. This check runs only for the database breaker.</p><p><b>Database</b>: <code>circuit_breaker.open_timeout</code> (default: 15s).</p><p>The same setting means something else for the other breakers:<br><b>Backend</b>: <code>backend_circuit_breaker.open_timeout</code> (default: 5m, applies only when <code>backend_circuit_breaker.enabled: true</code>) is the delay before the first health check.<br><b>Redis</b>: <code>redis.open_timeout</code> (default: 15s) has no effect on recovery; Redis is PINGed every 5s while in fallback.</p><p>A random <code>probeJitter</code> of up to <code>openTimeout/4</code> is added on each open transition to prevent multiple circuit breakers from probing simultaneously after a shared failure event.</p><p>Until the timeout + jitter elapses, all requests receive the sentinel error without any I/O.</p>'
    },
    SENTINEL: {
      title: 'Return Sentinel Error',
      badge: 'reject', badgeText: 'fast rejection',
      body: '<p>Returns the circuit-specific sentinel error immediately, with no I/O to the underlying system.</p><p><b>Backend</b>: <code>ErrBackendUnavailable</code> &mdash; a write fails over to the next eligible backend, and a read fails over to the next replica.<br><b>Database</b>: <code>ErrDBUnavailable</code> &mdash; triggers degraded mode: writes return 503, reads fan out to all backends.<br><b>Redis</b>: an internal <code>redis unavailable</code> error &mdash; the counter backend switches to local in-memory counters.</p><p>The registry\'s <code>ExcludeUnhealthy</code> drops every backend whose breaker is open before routing, so most open-circuit rejections happen at the routing layer rather than here.</p>'
    },
    PROBE: {
      title: 'Probe Slot Available? (database only)',
      badge: 'decision', badgeText: 'atomic CAS',
      body: '<p>Database breaker only. <code>cb.probeInFlight.CompareAndSwap(false, true)</code> &mdash; atomic compare-and-swap ensures exactly <b>one</b> probe request passes through at a time.</p><p>If a probe is already in flight (another goroutine won the CAS), this request gets the sentinel error. This prevents a thundering herd of probe requests when multiple goroutines detect the timeout simultaneously.</p><p>The <code>probeInFlight</code> flag is cleared in both <code>onSuccess()</code> (probe succeeded, circuit closes) and <code>onFailure()</code> (probe failed, circuit re-opens).</p>'
    },
    HALFOPEN: {
      title: 'Transition to Half-Open (database only)',
      badge: 'process', badgeText: 'state transition',
      body: '<p><code>cb.transition(StateHalfOpen)</code> &mdash; allows exactly one real query through to test whether the database has recovered. Backend and Redis breakers never enter this state.</p><p>Logs: <code>"half-open: probing"</code> with <code>component=circuit_breaker</code>, <code>breaker_name</code>, and <code>open_duration</code> showing how long the circuit was open.</p><p class="ac-metric">Metric: s3o_circuit_breaker_transitions_total{name, from="open", to="half-open"}<br>Gauge: s3o_circuit_breaker_state{name} = 2</p>'
    },
    POST: {
      title: 'PostCheck / Error Filter',
      badge: 'decision', badgeText: 'result evaluation',
      body: '<p><code>cb.PostCheck(err)</code> passes the operation result through the pluggable error filter (<code>cb.isError(err)</code>) to decide whether it counts as a circuit-breaker failure.</p><p><b>Backend filter</b> (<code>isBackendError</code>): counts only errors with no HTTP status (connection refused, DNS, TLS, reset), 5xx, 429, 401 and 403. Other statuses, such as a missing key or a failed precondition, do not count. Context cancellation and deadline errors do not count either.</p><p><b>Database filter</b> (<code>isDBError</code>): exempts application-level errors (<code>S3Error</code>, <code>ErrNoSpaceAvailable</code>) and the no-rows sentinels (<code>sql.ErrNoRows</code>, <code>pgx.ErrNoRows</code>). Every other error trips the breaker.</p><p>If the error passes the filter, flows to failure handling. If not (or nil), flows to success.</p>'
    },
    SUCCESS: {
      title: 'Return Result',
      badge: 'success', badgeText: 'success',
      body: '<p>The operation succeeded (or the error was not a circuit-breaker error). Returns the result to the caller.</p><p>If the circuit was closed, the failure counter was reset to 0 by <code>onSuccess()</code>. The circuit remains closed and all subsequent requests continue passing through normally.</p>'
    },
    FAIL: {
      title: 'Increment Failures',
      badge: 'filter', badgeText: 'failure tracking',
      body: '<p><code>cb.onFailure()</code> increments <code>cb.failures++</code> and records <code>cb.lastFailure = time.Now()</code>.</p><p>The failure counter tracks <b>consecutive</b> failures. It is reset to 0 on any successful call via <code>onSuccess()</code>. This means intermittent errors (occasional timeouts in otherwise healthy traffic) do not trip the breaker.</p><p>The <code>lastFailure</code> timestamp is used by the database breaker to calculate when the <code>openTimeout</code> elapses for probe eligibility.</p>'
    },
    WASHO: {
      title: 'Was Half-Open? (database only)',
      badge: 'decision', badgeText: 'probe result',
      body: '<p>Only the database breaker can be half-open. If it was in <code>StateHalfOpen</code> when the failure occurred, the probe query failed &mdash; the database is still down.</p><p>The <code>probeInFlight</code> atomic flag is cleared (<code>Store(false)</code>) so a future probe can be attempted after the open timeout elapses again.</p><p>The circuit transitions directly back to open without waiting for the failure threshold.</p>'
    },
    REOPEN: {
      title: 'Transition to Open (probe failed, database only)',
      badge: 'reject', badgeText: 'state transition',
      body: '<p><code>cb.transition(StateOpen)</code> &mdash; the database probe failed, circuit re-opens. The <code>lastFailure</code> timestamp resets the open timeout window, and a new jitter is drawn.</p><p>Logs: <code>"reopened: probe failed"</code> with <code>component=circuit_breaker</code>, <code>breaker_name</code>, and the current failure count.</p><p>The cycle repeats: after <code>openTimeout</code> plus jitter elapses, another single probe is attempted.</p><p class="ac-metric">Metric: s3o_circuit_breaker_transitions_total{name, from="half-open", to="open"}<br>Gauge: s3o_circuit_breaker_state{name} = 1</p>'
    },
    THRESH: {
      title: 'Failures >= Threshold?',
      badge: 'decision', badgeText: 'threshold check',
      body: '<p>Compares <code>cb.failures >= cb.failThreshold</code>.</p><p>Configurable thresholds:<br><b>Database</b>: <code>circuit_breaker.failure_threshold</code> (default: 3)<br><b>Backend</b>: <code>backend_circuit_breaker.failure_threshold</code> (default: 5)<br><b>Redis</b>: <code>redis.failure_threshold</code> (default: 3)</p><p>Backend values apply only when <code>backend_circuit_breaker.enabled: true</code> (default: false).</p><p>Below threshold, the error is returned to the caller as-is (not replaced with the sentinel), allowing the caller to handle it normally while the breaker continues tracking.</p>'
    },
    PASSTHRU: {
      title: 'Return Original Error',
      badge: 'process', badgeText: 'below threshold',
      body: '<p>Failure count is below the threshold. The original error is returned to the caller unchanged. The circuit remains closed.</p><p>The caller handles the error normally &mdash; for backends, this may trigger write failover to another backend. For database operations, the error propagates to the HTTP handler.</p><p>The failure counter persists: if the next call also fails, it increments further toward the threshold. A single success resets the counter to 0.</p>'
    },
    TRIP: {
      title: 'Transition to Open (threshold reached)',
      badge: 'reject', badgeText: 'state transition',
      body: '<p><code>cb.transition(StateOpen)</code> &mdash; consecutive failure threshold reached. The circuit opens, and <code>cb.openedAt</code> is recorded for duration tracking.</p><p>Logs: <code>"opened: failure threshold reached"</code> with <code>component=circuit_breaker</code>, <code>breaker_name</code>, failure count, threshold and the last error.</p><p>PostCheck wraps the sentinel around the original error with <code>fmt.Errorf("%w: %w", sentinel, err)</code>. Callers match the sentinel with <code>errors.Is</code>, and logs still show what actually failed.</p><p class="ac-metric">Metric: s3o_circuit_breaker_transitions_total{name, from="closed", to="open"}<br>Gauge: s3o_circuit_breaker_state{name} = 1</p>'
    },
    RECOVER: {
      title: 'Transition to Closed (database only)',
      badge: 'success', badgeText: 'state transition',
      body: '<p><code>cb.transition(StateClosed)</code> &mdash; the database probe succeeded, the database is healthy again.</p><p>Logs: <code>"closed: recovered"</code> with <code>component=circuit_breaker</code>, <code>breaker_name</code>, and <code>degraded_duration</code> showing total time spent in open + half-open states.</p><p>The <code>probeInFlight</code> flag is cleared and all subsequent requests pass through normally.</p><p class="ac-metric">Metric: s3o_circuit_breaker_transitions_total{name="database", from="half-open", to="closed"}<br>Gauge: s3o_circuit_breaker_state{name} = 0</p>'
    },
    STALE: {
      title: 'Probe Stale? (database only)',
      badge: 'decision', badgeText: 'stale probe check',
      body: '<p>While the database breaker is half-open, every other call gets the sentinel error. PreCheck also checks how long the current probe has been in flight.</p><p>If it has been 2 minutes or more (<code>probeTimeout</code>), the probe is treated as abandoned and the breaker resets to open. Otherwise the call is refused and the probe keeps running.</p>'
    },
    REOPEN2: {
      title: 'Reset to Open (stale probe, database only)',
      badge: 'reject', badgeText: 'state transition',
      body: '<p>A half-open probe that has been in flight for 2 minutes or more is treated as abandoned, for example when PostCheck was never called. The breaker clears the probe flag and goes back to open, so a later request can probe again.</p><p>PreCheck does this when a request arrives. The breaker watchdog also does it on its 5s tick through <code>ResetStaleProbe()</code>, so a stale probe is cleared even with no traffic.</p><p class="ac-metric">Metric: s3o_circuit_breaker_transitions_total{name="database", from="half-open", to="open"}</p>'
    },
    WATCHDOG: {
      title: 'Backend and Redis Health Check Loop',
      badge: 'entry', badgeText: 'external recovery',
      body: '<p>Backend and Redis breakers never let a real call through as a probe. A separate loop checks the dependency while the breaker is open.</p><p><b>Backend</b>: the breaker watchdog ticks every 5s and calls <code>RecoveryProber.Probe</code> for each backend. The first health check runs <code>max(open_timeout, 5s)</code> after the circuit opens.</p><p><b>Redis</b>: a background goroutine PINGs Redis every 5s while the counter backend is in fallback.</p>'
    },
    HEALTH: {
      title: 'Health Check Passes?',
      badge: 'decision', badgeText: 'out-of-band check',
      body: '<p><b>Backend</b>: a <code>HeadBucket</code> call on the wrapped backend, bypassing the breaker, with a 10s timeout. Each check is admitted and charged against the backend\'s usage budget as <code>HeadBucket</code>. A check the budget would refuse is skipped until there is room.</p><p><b>Redis</b>: a <code>PING</code>. On success, the local counter deltas collected during the outage are replayed to Redis in one pipeline before the breaker closes.</p>'
    },
    BACKOFF: {
      title: 'Stay Open, Check Again Later',
      badge: 'reject', badgeText: 'still down',
      body: '<p>The circuit stays open and every call keeps getting the sentinel error.</p><p><b>Backend</b>: the wait before the next check doubles after each failure, capped at <code>max(5m, open_timeout)</code>. Logs: <code>"health check failed; circuit stays open"</code> with <code>next_check_in</code>.</p><p><b>Redis</b>: the next PING runs on the next 5s tick.</p>'
    },
    EXTRECOVER: {
      title: 'Recover: Open to Closed',
      badge: 'success', badgeText: 'state transition',
      body: '<p><code>cb.Recover()</code> moves the breaker straight from open to closed, with no half-open step. It clears probe state and resets the failure counter, so the breaker again tolerates the configured threshold of failures.</p><p>Logs: <code>"closed: recovered"</code> with <code>component=circuit_breaker</code>, <code>breaker_name</code>, and <code>degraded_duration</code>.</p><p class="ac-metric">Metric: s3o_circuit_breaker_transitions_total{name, from="open", to="closed"}<br>Gauge: s3o_circuit_breaker_state{name} = 0</p>'
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
| <span style="color:#1a7a5a">**Forest green**</span> | Entry point |
| <span style="color:#c4a35a">**Amber**</span> | Failure tracking |
| <span style="color:#2a9d73">**Green border**</span> | Decision / branch |
| <span style="color:#5ec9a0">**Teal**</span> | Processing step |
| <span style="color:#34b882">**Green**</span> | Success / recovery |
| <span style="color:#d4a0a0">**Red**</span> | Rejection / circuit open |

