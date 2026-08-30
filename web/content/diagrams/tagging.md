---
title: "Object Tagging"
linkTitle: "Object Tagging"
weight: 9
---

Object tagging: the four ways a tag set is written, the one place it is stored, the two ways it is read back, and what clears it. **Hover over any component** for implementation details.

### How it works

A tag set is key/value labels attached to an object independently of its data. Tags are yours to give meaning to: the one place the orchestrator acts on a tag itself is [lifecycle expiration](../../docs/cleanup-and-lifecycle/#lifecycle-object-expiration), where a rule can filter on one. Otherwise they are stored, shared by every replica, and served back as written, and no key is treated specially.

The set is stored once per object key, never per copy. An object exists as N replicas with no authoritative copy, so per-replica rows would let three copies of a key disagree with nothing to say which one wins. That also keeps tagging off the providers entirely, which matters because provider support is inconsistent and a backend sitting over its usage limit could not be tagged at all.

#### Four ways in, two ways out

`PutObject` and `CreateMultipartUpload` take the set inline as `x-amz-tagging`, query-string encoded. `PutObjectTagging` takes it as a `Tagging` XML document on the `?tagging` subresource. `CopyObject` either carries the source's set or replaces it, depending on `x-amz-tagging-directive`. All four converge on the same validation and the same rows.

Reads split by what the caller needs. `GetObjectTagging` returns the tags themselves and is the only way to see them. `GetObject` and `HeadObject` report just how many, in `x-amz-tagging-count`, so a client can decide whether that second round trip is worth making.

#### Why validation happens early

An inline header is parsed and checked before the request body is read. Refusing later means the bytes have already been transferred and written, leaving an orphan to collect and the ingress already spent. For a multipart upload the same logic applies across a longer window: the set is validated at create, so an upload that would end in a rejected tag set is refused before any part is transferred rather than after all of them.

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
    '    INLINE([PutObject<br>x-amz-tagging]):::entry --> VALIDATE',
    '    MPUC([CreateMultipartUpload<br>x-amz-tagging]):::entry --> VALIDATE',
    '    SUB([PutObjectTagging<br>?tagging document]):::entry --> VALIDATE',
    '    COPY([CopyObject<br>tagging directive]):::entry --> VALIDATE',
    '',
    '    VALIDATE{"Valid set?<br>10 tags, 128 / 256"}:::filter -->|no| REJECT[400 InvalidTag<br>or BadRequest]:::reject',
    '    VALIDATE -->|"yes, multipart"| HOLD[Hold on<br>multipart_uploads.tagging]:::process',
    '    VALIDATE -->|yes| LOCK[Lock the object key]:::process',
    '',
    '    HOLD --> DONE([CompleteMultipartUpload]):::entry',
    '    DONE --> LOCK',
    '',
    '    LOCK --> EXISTS{Key holds<br>a copy?}:::decision',
    '    EXISTS -->|no| NOKEY[404 NoSuchKey]:::reject',
    '    EXISTS -->|yes| REPLACE[Delete existing rows,<br>insert the new set]:::process',
    '    REPLACE --> TAGS',
    '',
    '    DROP([PutObject overwrite<br>DeleteObject<br>last copy removed]):::entry --> CLEAR[Clear the set in the<br>object transaction]:::process',
    '    CLEAR --> TAGS[("object_tags<br>one set per object key")]:::storage',
    '',
    '    GETT([GetObjectTagging]):::entry --> READ[Read the set<br>for the key]:::storage',
    '    TAGS --> READ',
    '    READ --> SORT[Sort by tag key]:::process',
    '    SORT --> SERVE[TagSet to client<br>200, empty if none]:::success',
    '',
    '    GETO([GetObject<br>HeadObject]):::entry --> COUNT[Count the set<br>for the key]:::storage',
    '    TAGS --> COUNT',
    '    COUNT --> HDR[x-amz-tagging-count<br>omitted when zero]:::success',
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

  mermaid.render('tag-mermaid-svg', diagramSrc).then(function(result) {
    document.getElementById('ac-diagram').innerHTML = result.svg;
    wireUpInteractivity();
  });

  var nodeInfo = {
    INLINE: {
      title: 'PutObject with x-amz-tagging',
      badge: 'entry', badgeText: 'entry point',
      body: '<p>The header is query-string encoded (<code>k1=v1&amp;k2=v2</code>), not the XML document the tagging endpoints exchange.</p><p>Parsed and validated before the request body is read, so an unusable set costs no ingress and leaves no orphan to collect. The tags are then written in the object\'s own transaction, so there is no window where the object exists untagged.</p><p>A repeated key survives <code>url.ParseQuery</code> as extra slice entries rather than an error, so the duplicate is caught explicitly instead of silently keeping whichever came first.</p>'
    },
    MPUC: {
      title: 'CreateMultipartUpload with x-amz-tagging',
      badge: 'entry', badgeText: 'entry point',
      body: '<p>Same header, but the set has to survive until the upload completes, which may be hours and many parts later.</p><p>Validated here rather than at completion: an upload that would end in a rejected tag set is refused before any part is transferred rather than after all of them.</p>'
    },
    SUB: {
      title: 'PutObjectTagging',
      badge: 'entry', badgeText: 'entry point',
      body: '<p>The <code>?tagging</code> subresource, carrying a <code>Tagging</code> XML document. Replaces the whole set rather than merging into it, so adding one tag to an object that has three means sending all four.</p><p>An empty <code>TagSet</code> removes every tag, which the spec defines as the same outcome as <code>DeleteObjectTagging</code>.</p><p>The root element is pinned, so a document rooted at anything else is refused rather than silently decoding into an empty set. Bodies are capped at 64 KiB.</p>'
    },
    COPY: {
      title: 'CopyObject and x-amz-tagging-directive',
      badge: 'entry', badgeText: 'entry point',
      body: '<p>Absent or <code>COPY</code> carries the source object\'s tag set to the destination. <code>REPLACE</code> ignores it and takes the set from the copy request\'s own <code>x-amz-tagging</code> header.</p><p>Any other value is refused with <code>400 InvalidArgument</code> rather than quietly treated as <code>COPY</code>: falling back would put the source\'s tags on a copy the client asked to have different ones, which is the opposite of what it requested.</p>'
    },
    VALIDATE: {
      title: 'Tag set validation',
      badge: 'filter', badgeText: 'validation',
      body: '<p>The AWS limits, enforced on every path: 10 tags per object, 128 for a key, 256 for a value.</p><p>Lengths count <b>UTF-16 code units</b>, not runes or bytes, because S3 represents tags internally in UTF-16 where a character occupies one or two positions. A key of astral-plane characters therefore reaches the limit in half as many characters as a Latin one.</p><p>Keys and values are both case sensitive. A key must not be empty, and a set must not repeat one. An empty set is valid: it is how <code>PutObjectTagging</code> expresses a delete.</p><p>Callers validate before opening a transaction, so a rejected set costs no lock.</p>'
    },
    REJECT: {
      title: 'Validation refused',
      badge: 'reject', badgeText: 'rejected',
      body: '<p>Too many tags answers <code>400 BadRequest</code>. An empty key, an oversized key or value, a duplicate key, or an undecodable <code>x-amz-tagging</code> answers <code>400 InvalidTag</code>. A directive that is neither <code>COPY</code> nor <code>REPLACE</code> answers <code>400 InvalidArgument</code>.</p><p>Each message names the offending measurement, so a refusal says which limit was exceeded and by how much rather than a bare "invalid".</p>'
    },
    HOLD: {
      title: 'multipart_uploads.tagging',
      badge: 'process', badgeText: 'deferred',
      body: '<p>One column on the upload row rather than a child table. Unlike <code>object_tags</code>, these are only ever read whole for a single <code>upload_id</code> and never filtered by tag, so there is nothing for an index on <code>(tag_key, tag_value)</code> to serve.</p><p>Stored query-string encoded, the same shape the header uses. Nullable, and NULL means the upload carried no tags, which is what every pre-existing row is.</p><p>Aborting or completing the upload drops the row and the tags with it, which a child table would need its own cascade to match.</p><p><a href="../database-schema/">Database schema &rarr;</a></p>'
    },
    DONE: {
      title: 'CompleteMultipartUpload',
      badge: 'entry', badgeText: 'entry point',
      body: '<p>The held set is re-parsed and applied to the object the completion produces, in the same transaction that records the object. It was already validated at create, so what lands here has been checked.</p>'
    },
    LOCK: {
      title: 'Per-key advisory lock',
      badge: 'process', badgeText: 'concurrency',
      body: '<p>Every tag operation is keyed by object key alone and takes the key lock before touching a row, the same lock the write path uses.</p><p>On Postgres this is <code>pg_advisory_xact_lock</code>; on SQLite it is a no-op, since a single writer already serialises.</p><p>This is the operation that <code>make loadtest-tagging</code> concentrates on: drive it against a small seed to put several requests on the same keys.</p>'
    },
    EXISTS: {
      title: 'Does the key hold an object?',
      badge: 'decision', badgeText: 'guard',
      body: '<p>Tags belong to an object, so a key holding no copies has nothing to attach them to.</p><p>Checked inside the transaction, under the lock, rather than before it: a check outside would race with a concurrent delete of the last copy.</p>'
    },
    NOKEY: {
      title: '404 NoSuchKey',
      badge: 'reject', badgeText: 'rejected',
      body: '<p>Writing tags to a key that holds no copies is refused.</p><p><code>GetObjectTagging</code> on an object that exists but carries no tags is <b>not</b> this case: it answers <code>200</code> with an empty <code>TagSet</code>, because the object is there and simply has nothing on it. Clearing an already-empty set succeeds for the same reason.</p>'
    },
    REPLACE: {
      title: 'Replace the set',
      badge: 'process', badgeText: 'transaction',
      body: '<p>A delete of the key\'s existing rows followed by an insert of the new ones, inside one transaction. Replacement rather than merge, matching <code>PutObjectTagging</code>.</p><p>Ten tags per object caps how many rows a single key can add, which is what keeps the delete-then-insert cheap.</p>'
    },
    TAGS: {
      title: 'object_tags',
      badge: 'storage', badgeText: 'metadata store',
      body: '<p>One row per tag, keyed <code>(object_key, tag_key)</code>. Keyed by object key alone, not by backend: a tag set describes the object, so per-replica rows would let three copies of a key disagree with nothing to say which wins.</p><p>One row per tag rather than a JSON column on <code>object_locations</code>, because filtering objects by tag is a <code>WHERE tag_key = ? AND tag_value = ?</code>, which needs an index. A JSON blob turns that into a scan over every object. <code>idx_object_tags_lookup</code> on <code>(tag_key, tag_value)</code> serves that reverse direction; the primary key already serves lookup and delete by object key.</p><p>No foreign key, because there is no table to point at: <code>object_locations</code> is keyed <code>(object_key, backend_name)</code> and nothing is keyed on object key alone, so <code>ON DELETE CASCADE</code> cannot express this.</p><p><a href="../database-schema/">Database schema &rarr;</a></p>'
    },
    DROP: {
      title: 'What clears a tag set',
      badge: 'entry', badgeText: 'entry point',
      body: '<p>Tags follow the object, not the name. A key that stops holding the object it held stops carrying that object\'s tags.</p><p>Overwriting a key replaces the set with whatever the new request carried, so an untagged overwrite leaves the object untagged. Deleting an object clears its tags, by single delete or in a batch.</p><p>Deleting one location of an object that has several does <b>not</b> clear them; removing the last remaining copy does, because at that point the key no longer holds anything.</p>'
    },
    CLEAR: {
      title: 'Clear in the object transaction',
      badge: 'process', badgeText: 'cascade',
      body: '<p>Because there is no foreign key to cascade through, the store clears these rows explicitly, in the same transaction and under the same key lock as the write that orphaned them.</p><p>Doing it in the object\'s own transaction is what removes the window: there is no moment where a new object at a key carries the previous object\'s tags.</p><p><a href="../write-path/">Write path &rarr;</a></p>'
    },
    GETT: {
      title: 'GetObjectTagging',
      badge: 'entry', badgeText: 'entry point',
      body: '<p>Reads the stored set. Never reaches a backend, because the set lives in the metadata store rather than on any provider.</p><p>The admin API exposes the same set as JSON at <code>/admin/api/objects/tags/{key}</code>, under the same lock, for <code>adminctl</code>, the dashboard and the TUI.</p>'
    },
    READ: {
      title: 'Read the set',
      badge: 'storage', badgeText: 'metadata store',
      body: '<p>A lookup on the primary key, which already covers reads by object key, so no separate index is needed for this direction.</p><p>Every replica of the object shares this one set. There is no request that reaches only one backend\'s tags, because there is no such thing.</p>'
    },
    SORT: {
      title: 'Sort by tag key',
      badge: 'process', badgeText: 'wire format',
      body: '<p>Sorted so the response is byte-identical run to run. The store already orders its rows, but sorting at the transport means the wire format does not depend on that promise holding in both engines.</p>'
    },
    SERVE: {
      title: 'TagSet to the client',
      badge: 'success', badgeText: 'success',
      body: '<p><code>200</code> with the <code>Tagging</code> document. An untagged object gets an empty <code>TagSet</code> rather than a <code>404</code>.</p><p><code>DeleteObjectTagging</code> answers <code>204</code>. Both writes are recorded in the audit log, as <code>s3.PutObjectTagging</code> and <code>s3.DeleteObjectTagging</code>.</p><p><a href="../../docs/tagging/">Tagging reference &rarr;</a></p>'
    },
    GETO: {
      title: 'GetObject and HeadObject',
      badge: 'entry', badgeText: 'entry point',
      body: '<p>Every read reports how many tags the object carries, so a client can tell an object worth calling <code>GetObjectTagging</code> on from one that would answer with an empty set.</p><p>A <code>GET</code> served from the object data cache never reaches the metadata store, so its entry carries the count it was filled with, and a tag write drops the entry. <code>HEAD</code> does not use that cache and counts on every request.</p><p><a href="../read-path/">Read path &rarr;</a></p>'
    },
    COUNT: {
      title: 'Count the set',
      badge: 'storage', badgeText: 'metadata store',
      body: '<p>A <code>count(*)</code> on the primary key prefix, which is an index-only scan over the one key\'s rows. The read path needs the size of the set, not its contents.</p><p>A separate query rather than a subquery folded into the location lookup: that lookup is shared by the scrubber, drain, reconcile and the sync command, and a per-object count has no business on a per-copy row those callers read.</p>'
    },
    HDR: {
      title: 'x-amz-tagging-count',
      badge: 'success', badgeText: 'response header',
      body: '<p>Sent only when the object carries at least one tag. An untagged object omits the header rather than sending a zero, matching S3, so its presence reads as "there is a set here worth fetching".</p><p>Advisory, never authoritative. A count the store cannot serve is reported as none and the header left off, because the object\'s bytes are already correct and one header is not worth failing a read over. That is also what a degraded read does, having reached the object by broadcast with the store unreachable.</p><p><a href="../../docs/tagging/">Tagging reference &rarr;</a></p>'
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
| <span style="color:#c4a35a">**Amber**</span> | Validation |
| <span style="color:#2a9d73">**Green border**</span> | Decision / branch |
| <span style="color:#5ec9a0">**Teal**</span> | Processing step |
| <span style="color:#4aaa8a">**Teal**</span> | Metadata store |
| <span style="color:#34b882">**Green**</span> | Success |
| <span style="color:#d4a0a0">**Red**</span> | Rejected |

## See also

- [Tagging reference](../../docs/tagging/) - semantics, limits and the operator surface
- [Database schema](../database-schema/) - `object_tags` and the multipart `tagging` column
- [Write path](../write-path/) - where an inline set is parsed and written
- [Read path](../read-path/) - where the tagging count joins a GET or HEAD
