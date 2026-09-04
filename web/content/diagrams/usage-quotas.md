---
description: "Interactive diagram of how a backend call is admitted against its monthly budgets, what it charges, and where those counters live."
title: "Usage Quotas"
linkTitle: "Usage Quotas"
weight: 10
---

How a backend call is admitted against its monthly budgets, what it charges, and where those counters live. **Hover over any component** for implementation details.

### How it works

Every call the orchestrator makes to a backend is admitted against that backend's monthly budgets before it goes out, and recorded after. Three dimensions are enforced: **egress bytes**, **ingress bytes**, and **requests**. The first two are scalars. Requests are not, because providers do not bill them as one.

#### Why requests are pooled

Providers group operations into billing classes with separate allowances, and they disagree about the grouping. GCS meters uploads, copies and listings from a small Class A allowance, reads from a Class B allowance ten times its size, and does not bill deletes at all. B2 inverts it: uploads and deletes are free, downloads and listings are charged from two different classes. OCI meters every request from one pooled allowance. IDrive e2 does not meter requests at all.

Charging every operation against a single `api_request_limit` forces a bad choice: set it to the strictest class and the rest of the headroom is wasted, set it to the loosest and the strict one is blown. In production that second failure took a GCS backend out of service on a 5,000 upload allowance while its 50,000 read allowance sat 98.7% unused - and because the read path checks the same counter, the backend stopped serving reads too.

A backend therefore declares **pools**: named budgets, each covering a set of operations.

```yaml
unmetered: [DeleteObject, DeleteObjects, AbortMultipartUpload]
request_limits:
  - name: class_a
    operations: [PutObject, CopyObject, ListObjects, ListObjectsV2]
    limit: 5000
  - name: class_b
    operations: [GetObject, HeadObject, GetParts]
    limit: 50000
```

#### The rules

1. **Pools are additive.** An operation charges every pool containing it and is admitted only when all of them have headroom. That is what lets a per-operation sub-cap sit inside an aggregate one.
2. **`"*"` covers everything not listed as `unmetered`.** A bare `api_request_limit` desugars to exactly one such pool, named `all`, so configs written before pools existed keep their old meaning.
3. **`limit: 0` counts without refusing.** Same convention the byte limits use.
4. **Unmetered means uncharged, not unrecorded.** A free operation still increments the backend's request total; it simply charges no pool. Not being billed for a call is not a reason to stop reporting that it happened.
5. **Deletes are never refused**, on any budget. Refusing one would leave an operator over a limit with no way back under it, and a client `DELETE` that returns without deleting is simply wrong.

Because pools overlap, their counts do not sum to the request total and are not a decomposition of it.

#### What a refusal looks like

Admission is the same check on both paths; only the consequence differs. A **write** filters over-budget backends out of placement, so the object lands on another backend, or the client gets `507 Insufficient Storage` when none is eligible. A **read** skips over-budget copies and fails over to another; when every copy is on an over-budget backend the client gets `429 Too Many Requests` with `SlowDown`.

#### Where the counters live

A charge lands in the counter backend first - local atomics, or Redis when it is configured for multi-instance deployments - and is flushed to Postgres every 30 seconds, or every 10 when a budget is close to its ceiling. Admission reads **baseline + unflushed**: the DB figure the metrics collector last loaded, plus what this process has spent since. Enforcement is deliberately approximate, bounded by one flush interval of concurrent traffic, because exact enforcement would need a lock on every request.

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
    '    CALL([Backend call<br>client request or worker pass]):::entry --> OPNAME[Name the Operation<br>s3op.Operation]:::process',
    '',
    '    OPNAME --> ISDEL{Delete?}:::decision',
    '    ISDEL -->|yes| UNGATED[Never Refused]:::success',
    '    ISDEL -->|no| RESOLVE[Resolve the Pools<br>this Operation Charges]:::process',
    '',
    '    RESOLVE --> UNMET{Listed as<br>unmetered?}:::decision',
    '    UNMET -->|yes| NOPOOL[Charged to<br>No Budget]:::process',
    '    UNMET -->|no| POOLCHK{Every charged pool<br>has headroom?}:::filter',
    '',
    '    POOLCHK -->|no| REFUSE[Backend Refused]:::reject',
    '    POOLCHK -->|yes| BYTECHK{Egress and ingress<br>within limits?}:::filter',
    '    BYTECHK -->|no| REFUSE',
    '    BYTECHK -->|yes| ADMIT[Admitted]:::success',
    '',
    '    REFUSE --> WRITEOUT[Write: next backend,<br>or 507 if none]:::reject',
    '    REFUSE --> READOUT[Read: next copy,<br>or 429 SlowDown]:::reject',
    '',
    '    ADMIT --> CALLBE[Call the Backend]:::storage',
    '    UNGATED --> CALLBE',
    '    NOPOOL --> CALLBE',
    '',
    '    CALLBE --> RECORD[Record: request total<br>plus each charged pool]:::process',
    '    RECORD --> COUNTER[Counter Backend<br>atomics or Redis]:::storage',
    '    COUNTER --> FLUSH[Usage Flusher<br>every 30s]:::process',
    '    FLUSH --> TABLES[backend_usage and<br>backend_request_usage]:::storage',
    '    TABLES --> BASELINE[Collector Reloads<br>Baselines]:::process',
    '    BASELINE --> POOLCHK',
    '',
    '    classDef entry fill:#1a7a5a,stroke:#1a7a5a,color:#fff,font-weight:bold',
    '    classDef filter fill:#6b5b2e,stroke:#c4a35a,color:#fff',
    '    classDef decision fill:#1e2a26,stroke:#2a9d73,color:#e6edf3,font-size:11px',
    '    classDef process fill:#2d7d6a,stroke:#5ec9a0,color:#fff',
    '    classDef storage fill:#1a3a30,stroke:#4aaa8a,color:#c9d1d9',
    '    classDef success fill:#1a7a5a,stroke:#34b882,color:#fff,font-weight:bold',
    '    classDef reject fill:#8b3a3a,stroke:#d4a0a0,color:#fff,font-weight:bold'
  ].join('\n');

  mermaid.initialize({
    startOnLoad: false, theme: 'dark',
    flowchart: { nodeSpacing: 14, rankSpacing: 22, curve: 'basis', padding: 5, diagramPadding: 8, useMaxWidth: true }
  });

  mermaid.render('quota-mermaid-svg', diagramSrc).then(function(result) {
    document.getElementById('ac-diagram').innerHTML = result.svg;
    wireUpInteractivity();
  });

  var nodeInfo = {
    CALL: {
      title: 'Backend Call',
      badge: 'entry', badgeText: 'entry point',
      body: '<p>Every call that reaches a provider passes through here, whatever asked for it: a client GET or PUT, a replication copy, a rebalance move, a scrub read, a reconcile listing page, or one of the bulk rewrite passes.</p><p>Client traffic and background work draw on the same budgets deliberately. A fleet-wide pass is the largest consumer of a metered backend, and one that spent freely while client reads were refused on the counter it ran up would be the wrong way round.</p><p><a href="../write-path/">Write path &rarr;</a> &middot; <a href="../read-path/">Read path &rarr;</a></p>'
    },
    OPNAME: {
      title: 'Name the Operation',
      badge: 'process', badgeText: 'classification',
      body: '<p>The charge carries which operation it is - <code>s3op.Operation</code>, a closed set of thirteen - rather than a count of calls.</p><p>A count cannot be priced. Providers bill an upload and a read from different allowances and some operations not at all, so "one API call" is not enough information to settle the charge. The same vocabulary is what config validates operation names against and what the per-operation metric labels use, so the three cannot drift apart.</p><p>Operations: <code>PutObject</code>, <code>GetObject</code>, <code>HeadObject</code>, <code>DeleteObject</code>, <code>DeleteObjects</code>, <code>CopyObject</code>, <code>ListObjects</code>, <code>ListObjectsV2</code>, <code>CreateMultipartUpload</code>, <code>UploadPart</code>, <code>CompleteMultipartUpload</code>, <code>AbortMultipartUpload</code>, <code>GetParts</code>.</p>'
    },
    ISDEL: {
      title: 'Delete?',
      badge: 'decision', badgeText: 'gate exemption',
      body: '<p>Deletes are recorded but never gated, whatever any budget says.</p><p>A delete is the one operation that reduces what a backend holds. Refusing it over budget would trap an operator above a limit with no way back under it, and a client <code>DELETE</code> that returns success without removing the object is simply incorrect.</p><p>The same exemption covers the internal delete paths: cleanup-queue retries, drain purges, and lifecycle expiry.</p>'
    },
    UNGATED: {
      title: 'Never Refused',
      badge: 'success', badgeText: 'always admitted',
      body: '<p>The call proceeds regardless of remaining budget, and is still charged afterwards.</p><p>Whether it charges a pool is a separate question, answered by <code>unmetered</code>: on GCS, where deletes are free, an operator lists them there and the delete charges nothing but the request total. On a provider that does bill deletes, leaving them out of <code>unmetered</code> charges them normally - they just cannot be refused.</p>'
    },
    RESOLVE: {
      title: 'Resolve the Pools',
      badge: 'process', badgeText: 'lookup',
      body: '<p>Config compiles once at startup and on reload into a map from operation to the pools charging it, so this is a map lookup rather than matching names against pool definitions on every request.</p><p>An operation can belong to several pools. Pools are <b>additive</b>: it charges each of them and needs headroom in all of them, which is what lets a sub-cap on one operation sit inside an aggregate cap over everything.</p><p><a href="../../docs/backends/">Backend configuration &rarr;</a></p>'
    },
    UNMET: {
      title: 'Listed as unmetered?',
      badge: 'decision', badgeText: 'billing check',
      body: '<p><code>unmetered</code> names the operations the provider does not bill at all. They are removed from every pool, including a <code>"*"</code> wildcard, which would otherwise swallow them.</p><p>This is not the same as being ungated. An unmetered operation charges no budget; an ungated one cannot be refused. A delete on GCS is both; a delete on a provider that bills them is only the second.</p>'
    },
    NOPOOL: {
      title: 'Charged to No Budget',
      badge: 'process', badgeText: 'recorded, unbilled',
      body: '<p>The call still increments the backend\'s request total in <code>backend_usage.api_requests</code>. It simply charges no pool, so no budget moves toward refusing anything.</p><p>Recording it matters: the total is the honest answer to "how much did we use this backend", and an operator reading a request count should see the requests that were made, not the subset someone is billed for.</p><p>This is the half of the old behaviour that was wrong in production. Roughly 47% of one backend\'s counted operations were deletes the provider gives away, and charging them is what exhausted its budget.</p>'
    },
    POOLCHK: {
      title: 'Pool Headroom',
      badge: 'filter', badgeText: 'admission',
      body: '<p>For each pool the operation charges: <code>baseline + unflushed + proposed &le; limit</code>. All of them must pass. A pool with <code>limit: 0</code> is counted and never refuses.</p><p>Effective usage is the DB baseline the metrics collector last loaded plus what this process has spent since the last flush. Enforcement is approximate by design - concurrent requests can all pass and collectively overshoot by up to one flush interval - because exact enforcement would need a lock on every request.</p><p>This is where a class split earns its keep: an upload budget can be spent while the read budget is untouched, and reads keep being served.</p><p class="ac-metric">Metrics: s3o_usage_pool_requests{backend,pool}, s3o_usage_pool_limit{backend,pool}</p>'
    },
    BYTECHK: {
      title: 'Byte Limits',
      badge: 'filter', badgeText: 'admission',
      body: '<p><code>egress_byte_limit</code> and <code>ingress_byte_limit</code> are checked the same way, against the same baseline-plus-unflushed view.</p><p>These stay scalar because providers do not class bytes: a gigabyte out is a gigabyte out, whichever call moved it. Only requests needed pooling.</p><p>The size charged is the size that will actually cross the link - the encoded bytes for a compressed object, the envelope for an encrypted one - not the logical size the client sees.</p>'
    },
    REFUSE: {
      title: 'Backend Refused',
      badge: 'reject', badgeText: 'over budget',
      body: '<p>This backend cannot absorb the operation. Nothing is charged, because nothing was sent.</p><p>The refusal is per backend, not per request: a fleet is only out of budget when every eligible backend is, which is what makes overflow to another provider the normal response to one hitting its ceiling.</p><p class="ac-metric">Metric: s3o_usage_limit_rejections_total{operation,limit_type}</p>'
    },
    WRITEOUT: {
      title: 'Write: Overflow or 507',
      badge: 'reject', badgeText: 'write path',
      body: '<p>Over-budget backends are filtered out of placement before a target is picked, so a write simply lands somewhere else.</p><p>When no backend is eligible the client gets <code>507 Insufficient Storage</code>, and the pre-flight <code>CanAcceptWrite</code> check makes that answer arrive before the body is transmitted rather than after.</p><p><a href="../admission-control/">Admission control &rarr;</a></p>'
    },
    READOUT: {
      title: 'Read: Failover or 429',
      badge: 'reject', badgeText: 'read path',
      body: '<p>A read skips the copy on an over-budget backend and tries the next one holding the object.</p><p>When every copy is on an over-budget backend the read fails with <code>ErrUsageLimitExceeded</code>, which the handler returns as <code>429 Too Many Requests</code> with the <code>SlowDown</code> S3 code - a retryable answer, since the budget resets.</p><p>This is why a single undifferentiated limit was worse than it looked: exhausting it did not merely stop writes, it made a backend unreadable as a serving copy.</p>'
    },
    ADMIT: {
      title: 'Admitted',
      badge: 'success', badgeText: 'within budget',
      body: '<p>Every pool the operation charges has room, and both byte dimensions do too.</p><p>Admission and accounting live on one type (<code>accounting.Recorder</code>) so a caller holding it can always ask before spending. They were once separate surfaces, and every path recorded what it spent while only some asked first, which kept the counters truthful while the budget was spent unchecked.</p>'
    },
    CALLBE: {
      title: 'Call the Backend',
      badge: 'storage', badgeText: 'S3 API call',
      body: '<p>The HTTP request goes out to the provider.</p><p>It is charged whatever the outcome. A call that failed still reached the provider and still counts against the allowance, so recording only successes would drift the ledger away from the bill.</p>'
    },
    RECORD: {
      title: 'Record the Charge',
      badge: 'process', badgeText: 'accounting',
      body: '<p>One charge updates both views: <code>+1</code> on the backend\'s request total, and <code>+1</code> on every pool the operation belongs to. A rewrite pass that reads and writes one object charges a read pool and a write pool, not two of whichever pool a scalar count would have landed in.</p><p>Bytes are added on the same call: egress for a successful GET-like operation, ingress for a PUT-like one.</p><p>Because pools overlap, their counts do not sum to the request total and must never be presented as if they did.</p>'
    },
    COUNTER: {
      title: 'Counter Backend',
      badge: 'storage', badgeText: 'in-memory or Redis',
      body: '<p>Charges land in memory first: per-backend atomics by default, or Redis when configured, which shares counters across instances so a fleet enforces one budget rather than one per process.</p><p>Pool counters live in a single Redis hash per backend and period, so a flush can enumerate exactly the pools that were charged without scanning the keyspace or being told which pools config currently declares.</p><p>Redis failures fall back to the local counters and replay them on recovery, pools included.</p>'
    },
    FLUSH: {
      title: 'Usage Flusher',
      badge: 'process', badgeText: 'every 30s',
      body: '<p>Reads and resets the counters, then writes the deltas to Postgres. Interval shortens to <code>fast_interval</code> when any budget - a byte limit or a request pool - passes <code>adaptive_threshold</code>, because that is when enforcement accuracy starts to matter.</p><p>Totals and pool counts are flushed separately, and each half restores only what it failed to write. Putting back a pool delta after the totals had landed would double-count it on the next pass.</p><p><b>Advisory lock</b>: <code>LockUsageFlush = 1007</code>.</p><p><a href="../background-services/">Background services &rarr;</a></p>'
    },
    TABLES: {
      title: 'Postgres Counters',
      badge: 'storage', badgeText: 'monthly rows',
      body: '<p><code>backend_usage(backend_name, period, api_requests, egress_bytes, ingress_bytes)</code> holds the totals; <code>backend_request_usage(backend_name, period, pool, requests)</code> holds one row per budget.</p><p>Both are keyed by calendar month (<code>YYYY-MM</code>), so a period rolls over on its own with no reset job. Both use additive <code>ON CONFLICT</code> upserts, so several instances flushing at once converge instead of overwriting each other.</p><p>Bytes stay columnar because their dimensions are fixed; pool counts are keyed because their names come from config and change with it.</p><p><a href="../database-schema/">Database schema &rarr;</a></p>'
    },
    BASELINE: {
      title: 'Reload Baselines',
      badge: 'process', badgeText: 'closes the loop',
      body: '<p>The metrics collector reads both tables for the current period and seeds the tracker\'s baselines, which is what makes a restarted process pick up mid-month rather than starting from zero.</p><p>Both halves are loaded together: seeding the totals without the pool counts would admit work against a budget already spent.</p><p>Baselines reset on period rollover and when a backend is drained, since its rows are gone.</p>'
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
| <span style="color:#c4a35a">**Amber**</span> | Budget check |
| <span style="color:#2a9d73">**Green border**</span> | Decision / branch |
| <span style="color:#5ec9a0">**Teal**</span> | Processing step |
| <span style="color:#4aaa8a">**Teal**</span> | Counter storage |
| <span style="color:#34b882">**Green**</span> | Admitted |
| <span style="color:#d4a0a0">**Red**</span> | Refused |

## See also

- [Backend configuration](../../docs/backends/) - `request_limits`, `unmetered`, and the operation vocabulary
- [Maximizing free tiers](../../guides/maximizing-free-tiers/) - a worked GCS class split
- [Write path](../write-path/) and [read path](../read-path/) - where admission sits in each
- [Admission control](../admission-control/) - the storage-capacity pre-flight this runs alongside
- [Background services](../background-services/) - the usage flusher that drains these counters
- [Database schema](../database-schema/) - `backend_usage` and `backend_request_usage`
