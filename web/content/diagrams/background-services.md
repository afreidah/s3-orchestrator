---
title: "Background Services"
linkTitle: "Background Services"
weight: 6
---

Coordination of periodic background workers that maintain storage health, enforce replication, and persist counters. **Hover over any component** for implementation details.

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
    'flowchart LR',
    '    SCHED([Lifecycle\\nManager]):::entry --> REPL[Replicator]:::process',
    '    SCHED --> REBAL[Rebalancer]:::process',
    '    SCHED --> OVERREP[Over-Replication\\nCleaner]:::process',
    '    SCHED --> LIFECYCLE[Lifecycle\\nExpiration]:::process',
    '    SCHED --> MPCLEAN[Multipart\\nCleanup]:::process',
    '    SCHED --> FLUSH[Usage\\nFlusher]:::filter',
    '    SCHED --> CQWORKER[Cleanup Queue\\nWorker]:::cleanup',
    '',
    '    REPL -->|copy to| S3[S3\\nBackends]:::storage',
    '    REBAL -->|move between| S3',
    '    OVERREP -->|delete from| S3',
    '    LIFECYCLE -->|delete from| S3',
    '    MPCLEAN -->|abort on| S3',
    '    CQWORKER -->|retry on| S3',
    '',
    '    REPL -->|on failure| CQ{{Cleanup\\nQueue}}:::cleanup',
    '    REBAL -->|on failure| CQ',
    '    OVERREP -->|on failure| CQ',
    '    CQWORKER -->|fetch items| CQ',
    '',
    '    FLUSH -->|persist| PG[(PostgreSQL)]:::storage',
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
    flowchart: { nodeSpacing: 80, rankSpacing: 160, curve: 'basis', padding: 16, diagramPadding: 8, useMaxWidth: true }
  });

  mermaid.render('bg-mermaid-svg', diagramSrc).then(function(result) {
    document.getElementById('ac-diagram').innerHTML = result.svg;
    wireUpInteractivity();
  });

  var nodeInfo = {
    SCHED: {
      title: 'Lifecycle Manager (Scheduler)',
      badge: 'entry', badgeText: 'scheduler',
      body: '<p>Central service orchestrator from <code>internal/lifecycle</code>. Launches all background workers as supervised goroutines.</p><p>Each service implements <code>lifecycle.Service</code> with a <code>Run(ctx)</code> method. Most workers use <code>lockedTickerService</code> which wraps periodic execution behind PostgreSQL advisory locks (<code>pg_try_advisory_lock</code>) for leader election across instances.</p><p>Services are defined in <code>cmd/s3-orchestrator/services.go</code>. Hot-reloadable configs are stored as <code>atomic.Pointer</code> on the <code>BackendManager</code>.</p>'
    },
    REPL: {
      title: 'Replicator',
      badge: 'process', badgeText: 'every 5 min',
      body: '<p><code>Replicator.Replicate()</code> creates additional copies of under-replicated objects to reach the configured replication factor.</p><p><b>Interval</b>: default 5 minutes (configurable via <code>replication.worker_interval</code>).<br><b>Advisory lock</b>: <code>LockReplicator = 1002</code>.<br><b>Batch size</b>: configurable, queries <code>GetUnderReplicatedObjects()</code>.<br><b>Concurrency</b>: parallel via <code>workerpool.Run()</code>.</p><p>Runs a <b>startup pass</b> immediately on boot for catch-up. Excludes backends unhealthy longer than <code>unhealthy_threshold</code>. Uses <code>streamCopy()</code> for zero-buffer transfer. Conditional <code>RecordReplica()</code> DB insert guards against concurrent overwrites/deletes.</p><p>On copy failure or stale source: orphan cleaned via <code>deleteOrEnqueue()</code> with reason <code>replication_orphan</code>.</p><p class="ac-metric">Metrics: replication_copies_created_total, replication_runs_total, replication_errors_total, replication_duration_seconds, replication_pending</p>'
    },
    REBAL: {
      title: 'Rebalancer',
      badge: 'process', badgeText: 'every 6 hrs',
      body: '<p><code>Rebalancer.Rebalance()</code> moves objects between backends to optimize space distribution.</p><p><b>Interval</b>: default 6 hours (configurable via <code>rebalance.interval</code>).<br><b>Advisory lock</b>: <code>LockRebalancer = 1001</code>.<br><b>Guard</b>: skips if disabled or utilization spread &lt; <code>threshold</code>.</p><p><b>Strategies</b>:<br>&bull; <code>spread</code>: equalizes utilization ratios (most over-target sources &rarr; most under-target destinations)<br>&bull; <code>pack</code>: consolidates onto most-full backends, pulling from least-full</p><p>Each move: <code>streamCopy()</code> &rarr; <code>MoveObjectLocation()</code> (atomic CAS) &rarr; delete source. On DB failure: destination orphan cleaned via <code>deleteOrEnqueue()</code> with reason <code>rebalance_orphan</code>. Source delete failures use reason <code>rebalance_source_delete</code>.</p><p class="ac-metric">Metrics: rebalance_objects_moved, rebalance_bytes_moved, rebalance_runs_total, rebalance_duration_seconds, rebalance_skipped</p>'
    },
    OVERREP: {
      title: 'Over-Replication Cleaner',
      badge: 'process', badgeText: 'every 5 min',
      body: '<p><code>OverReplicationCleaner.Clean()</code> removes surplus copies that exceed the target replication factor.</p><p><b>Interval</b>: default 5 minutes (configurable via <code>replication.worker_interval</code>).<br><b>Advisory lock</b>: <code>LockOverReplication = 1008</code>.<br><b>Guard</b>: only runs when <code>factor > 1</code>.</p><p>Queries <code>GetOverReplicatedObjects()</code>, groups by key, then scores each copy:<br>&bull; Draining backend: score 0 (remove first)<br>&bull; Circuit-broken backend: score 1<br>&bull; Healthy backend: 2 + (1 - utilization ratio), range [2..3]</p><p>Lowest-scoring copies removed first. Uses <code>RemoveExcessCopy()</code> with <code>FOR UPDATE</code> row lock to prevent races with concurrent replicator/rebalancer. Physical delete via <code>deleteOrEnqueue()</code> with reason <code>over_replication</code>.</p><p class="ac-metric">Metrics: over_replication_removed_total, over_replication_runs_total, over_replication_errors_total, over_replication_pending, over_replication_duration_seconds</p>'
    },
    LIFECYCLE: {
      title: 'Lifecycle Expiration',
      badge: 'process', badgeText: 'every 1 hr',
      body: '<p><code>BackendManager.ProcessLifecycleRules()</code> evaluates TTL-based lifecycle rules and deletes expired objects.</p><p><b>Interval</b>: 1 hour.<br><b>Advisory lock</b>: <code>LockLifecycle = 1005</code>.<br><b>Guard</b>: only runs when lifecycle rules are configured.<br><b>Batch size</b>: <code>lifecycleBatchSize = 100</code> per rule.</p><p>For each rule: computes <code>cutoff = now - expiration_days * 24h</code>, queries <code>ListExpiredObjects(ctx, prefix, cutoff, 100)</code>, then calls the standard <code>DeleteObject()</code> path (quota decrement, cache invalidation, cleanup queue on failure).</p><p>Audit event: <code>lifecycle.delete</code> with key, prefix, expiration_days.</p><p class="ac-metric">Metrics: lifecycle_deleted_total, lifecycle_failed_total, lifecycle_runs_total{status=success|partial|error}</p>'
    },
    MPCLEAN: {
      title: 'Multipart Cleanup',
      badge: 'process', badgeText: 'every 1 hr',
      body: '<p><code>MultipartManager.CleanupStaleMultipartUploads()</code> aborts multipart uploads older than 24 hours.</p><p><b>Interval</b>: 1 hour.<br><b>Advisory lock</b>: <code>LockMultipartCleanup = 1004</code>.<br><b>Stale threshold</b>: 24 hours.</p><p>Queries <code>GetStaleMultipartUploads(ctx, 24h)</code> for uploads with <code>created_at</code> older than the threshold. Each stale upload is aborted via <code>AbortMultipartUpload()</code>, which deletes uploaded parts from S3 backends and removes the DB records.</p><p>Audit event: <code>storage.MultipartCleanup</code> with cleaned count and total stale count.</p>'
    },
    FLUSH: {
      title: 'Usage Flusher',
      badge: 'filter', badgeText: 'every 30s (adaptive)',
      body: '<p><code>UsageTracker.FlushUsage()</code> reads and resets in-memory atomic counters, then writes accumulated deltas (API requests, egress, ingress) to PostgreSQL.</p><p><b>Interval</b>: default 30 seconds (configurable via <code>usage_flush.interval</code>).<br><b>Adaptive mode</b>: when any backend exceeds <code>adaptive_threshold</code> ratio of its usage limit, interval shortens to <code>fast_interval</code> for higher enforcement accuracy.<br><b>Advisory lock</b>: <code>LockUsageFlush = 1007</code> (only when Redis counters are active).</p><p>Counters keyed by calendar month (<code>YYYY-MM</code>) for automatic period rollover. On DB error, deltas are added back to avoid data loss. Drained backends have counters discarded. Also refreshes <code>UpdateQuotaMetrics()</code> each tick.</p><p class="ac-metric">Metric: s3o_quota_used_bytes, s3o_quota_limit_bytes (per-backend gauges)</p>'
    },
    CQWORKER: {
      title: 'Cleanup Queue Worker',
      badge: 'cleanup', badgeText: 'every 1 min',
      body: '<p><code>CleanupWorker.ProcessCleanupQueue()</code> retries failed object deletions from the <code>cleanup_queue</code> table.</p><p><b>Interval</b>: 1 minute.<br><b>Advisory lock</b>: <code>LockCleanupQueue = 1003</code>.<br><b>Batch size</b>: 50 items per tick.<br><b>Concurrency</b>: configurable (default 10).</p><p><b>Backoff</b>: exponential <code>min(1m * 2^attempts, 24h)</code>.<br><b>Max attempts</b>: 10. Exhausted items remain in the table with <code>orphan_bytes</code> preserved for operator review.</p><p>Items are fed from all failure sites: <code>recordObjectOrCleanup</code>, <code>DeleteObject</code>, <code>UploadPart</code>, <code>CompleteMultipartUpload</code>, <code>AbortMultipartUpload</code>, rebalancer (3 sites), replicator. On success, <code>DecrementOrphanBytes()</code> frees the reserved quota.</p><p class="ac-metric">Metrics: cleanup_queue_enqueued_total{reason}, cleanup_queue_processed_total{status=success|retry|exhausted}, cleanup_queue_depth</p>'
    },
    PG: {
      title: 'PostgreSQL',
      badge: 'storage', badgeText: 'shared state',
      body: '<p>Central metadata store shared by all background services. Hosts object locations, quota stats, usage counters, multipart upload state, cleanup queue, and advisory locks.</p><p><b>Advisory locks</b> provide leader election: <code>pg_try_advisory_lock(lockID)</code> ensures only one instance runs each service. Lock IDs: Rebalancer=1001, Replicator=1002, CleanupQueue=1003, MultipartCleanup=1004, Lifecycle=1005, UsageFlush=1007, OverReplication=1008.</p><p>Key tables: <code>object_locations</code>, <code>backend_quotas</code>, <code>backend_usage</code>, <code>cleanup_queue</code>, <code>multipart_uploads</code>, <code>multipart_parts</code>.</p>'
    },
    S3: {
      title: 'S3 Backends',
      badge: 'storage', badgeText: 'object storage',
      body: '<p>Physical storage backends (OCI Object Storage, Cloudflare R2, etc.) accessed through the <code>ObjectBackend</code> interface, optionally wrapped with <code>CircuitBreakerBackend</code>.</p><p>Background services interact via:<br>&bull; <code>streamCopy()</code>: piped <code>GetObject</code> &rarr; <code>PutObject</code> for rebalancer and replicator<br>&bull; <code>deleteWithTimeout()</code>: bounded <code>DeleteObject</code> for cleanup worker, lifecycle, over-replication<br>&bull; <code>AbortMultipartUpload()</code>: deletes uploaded parts for stale upload cleanup</p><p>All S3 API calls are recorded against per-backend usage counters (<code>usage.Record()</code>) for quota enforcement.</p>'
    },
    CQ: {
      title: 'Cleanup Queue Table',
      badge: 'cleanup', badgeText: 'retry queue',
      body: '<p>PostgreSQL table <code>cleanup_queue</code> storing failed deletion operations for background retry.</p><p><b>Schema</b>: <code>id</code>, <code>backend_name</code>, <code>object_key</code>, <code>reason</code>, <code>size_bytes</code>, <code>attempts</code>, <code>last_error</code>, <code>next_retry_at</code>, <code>created_at</code>.</p><p><b>Enqueue reasons</b>: <code>overwrite_displaced</code>, <code>rebalance_orphan</code>, <code>rebalance_stale_orphan</code>, <code>rebalance_source_delete</code>, <code>replication_orphan</code>, <code>over_replication</code>, <code>delete_failed</code>, <code>multipart_abort</code>.</p><p>Items enqueued via <code>enqueueCleanup()</code> which also calls <code>IncrementOrphanBytes()</code> on the backend quota to prevent over-allocation. The cleanup worker decrements orphan bytes on successful deletion.</p><p>SQL filter: <code>WHERE attempts < 10 AND next_retry_at <= now()</code> prevents exhausted items from being re-fetched while keeping them visible for operator intervention.</p>'
    },
    PG: {
      title: 'PostgreSQL',
      badge: 'storage', badgeText: 'shared state',
      body: '<p>Central metadata store shared by all background services. Hosts object locations, quota stats, usage counters, multipart upload state, cleanup queue, and advisory locks.</p><p><b>Advisory locks</b> provide leader election: <code>pg_try_advisory_lock(lockID)</code> ensures only one instance runs each service. Lock IDs: Rebalancer=1001, Replicator=1002, CleanupQueue=1003, MultipartCleanup=1004, Lifecycle=1005, UsageFlush=1007, OverReplication=1008.</p><p>All services query PG for work items and write back results. The Usage Flusher is the primary writer of counter data.</p>'
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
| <span style="color:#1f6feb">**Blue**</span> | Scheduler / entry point |
| <span style="color:#d29922">**Amber**</span> | Adaptive-interval service |
| <span style="color:#bc8cff">**Purple**</span> | Fixed-interval background worker |
| <span style="color:#79c0ff">**Dark blue**</span> | Shared storage (PostgreSQL / S3) |
| <span style="color:#8b949e">**Gray**</span> | Cleanup / retry queue |

## Service Summary

| Service | Interval | Advisory Lock ID | Key Function |
|---------|----------|------------------|--------------|
| Replicator | 5 min (configurable) | 1002 | `Replicator.Replicate()` |
| Rebalancer | 6 hrs (configurable) | 1001 | `Rebalancer.Rebalance()` |
| Over-Replication Cleaner | 5 min (configurable) | 1008 | `OverReplicationCleaner.Clean()` |
| Lifecycle Expiration | 1 hr | 1005 | `ProcessLifecycleRules()` |
| Multipart Cleanup | 1 hr | 1004 | `CleanupStaleMultipartUploads()` |
| Usage Flusher | 30s (adaptive) | 1007 (Redis only) | `FlushUsage()` |
| Cleanup Queue Worker | 1 min | 1003 | `ProcessCleanupQueue()` |

