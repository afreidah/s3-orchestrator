---
description: "Interactive diagram of at-rest compression: how an object is encoded on write, reassembled on read, and why the format is chunked."
title: "Compression"
linkTitle: "Compression"
weight: 6
---

At-rest compression: how an object is encoded on write and reassembled on read, and why the stored format is chunked. **Hover over any component** for implementation details.

### How it works

Objects are stored as **chunked zstd** in the Zstandard seekable format: one independently decodable frame per `chunk_size` of input, with a seek table in a trailing skippable frame. Sizes, ETags and content hashes stay those of the object the client wrote.

Storage and transfer are both metered on the backends this project targets, so compression reduces the bill twice. That second saving only holds if a partial read stays cheap, which is what drives the format.

#### Why the format is chunked

Compression emits backreferences into earlier data, so decoding can only begin at a frame boundary. One frame per object gives exactly one entry point, byte zero, so any range read would have to fetch the whole stored object and discard everything before the offset. The cost of a partial read becomes proportional to object size rather than to the bytes asked for, which is the wrong trade for a proxy whose backends meter egress.

Splitting into independently decodable frames gives one entry point per chunk, at the cost of an index and a small ratio penalty. At the 1 MiB default that penalty is about 2.5% on Go source and negative on JSON logs.

#### Writing an object

1. **Check the size floor**: objects below `min_size` are stored verbatim, because a seek table and per-frame headers cost more than a small object saves.
2. **Encode**: the buffered body is encoded into a second buffer, one frame per `chunk_size`, with the seek table appended.
3. **Check the ratio floor**: an encoding above `min_ratio` of the original size is discarded and the object stored verbatim. The decision is made by encoding and measuring, not by sampling, because entropy is not uniform across an object.
4. **Admit against the encoded size**: unlike encryption, whose overhead is a fixed function of the size, an encoder only reports its output once it has run. A compressed write is therefore admitted after encoding rather than before.
5. **Encrypt, if enabled**: compression runs first, in that order only, because ciphertext does not compress.
6. **Upload and record the stored form**: `compression_algorithm`, `compression_level`, `compression_format_version` and `logical_size`. A NULL algorithm is the ledger's way of saying the bytes are verbatim, so no separate boolean can drift out of step with it.

#### Reading an object

A compressed copy is never served by a whole-object GET. The codec drives the read instead:

1. **Read the seek table** from the trailing skippable frame, which maps a logical offset to the frame holding it.
2. **Map the client's range** onto the frames covering it.
3. **Fetch those frames** with one ranged backend GET each, decrypting them first when the copy is an envelope.
4. **Decode and slice** to the exact bytes the client asked for.

Each frame fetch is charged its own API call and egress, on the bytes that actually left the backend. Charging once per client request would under-report all but the first.

#### The two thresholds

`min_size` and `min_ratio` exist because an object can fail to benefit for two different reasons. `min_size` is about framing overhead on small objects; `min_ratio` is about entropy - already-compressed content, media and archives gain nothing at any size. Objects declined by either are stored exactly as the client sent them, and nothing on the read path distinguishes the two cases, because nothing needs to.

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
    '    PUT([PutObject or<br>Multipart Complete]):::entry --> MINSIZE{size >=<br>min_size?}:::filter',
    '',
    '    MINSIZE -->|no| VERBATIM[Store Verbatim]:::process',
    '    MINSIZE -->|yes| ENCODE[Encode: one zstd frame<br>per chunk_size]:::process',
    '    ENCODE --> SEEKTBL[Append Seek Table<br>skippable frame]:::process',
    '    SEEKTBL --> RATIO{encoded <= min_ratio<br>of original?}:::filter',
    '',
    '    RATIO -->|no| DISCARD[Discard Encoding]:::reject',
    '    DISCARD --> VERBATIM',
    '    RATIO -->|yes| ADMIT[Admit on<br>Encoded Size]:::filter',
    '',
    '    ADMIT --> ENCRYPT{Encryption<br>Enabled?}:::decision',
    '    ENCRYPT -->|yes| ENVELOPE[Encrypt the<br>Encoded Stream]:::process',
    '    ENCRYPT -->|no| UPLOAD',
    '    ENVELOPE --> UPLOAD[Upload to<br>Backend]:::storage',
    '    VERBATIM --> UPLOAD',
    '',
    '    UPLOAD --> ROW[Record Stored Form<br>on the Ledger Row]:::success',
    '',
    '    GETR([GetObject]):::entry --> ISCOMP{Row carries an<br>algorithm?}:::decision',
    '    ISCOMP -->|no| PLAINREAD[Whole-object GET]:::storage',
    '    ISCOMP -->|yes| RDTABLE[Fetch Seek Table]:::storage',
    '',
    '    RDTABLE --> MAPFRAME[Map Range to<br>Covering Frames]:::process',
    '    MAPFRAME --> FETCHF[Ranged GET<br>per Frame]:::storage',
    '    FETCHF --> DECR{Copy<br>Encrypted?}:::decision',
    '',
    '    DECR -->|yes| DECRYPTF[Decrypt Ciphertext<br>Chunks]:::process',
    '    DECR -->|no| DECODEF',
    '    DECRYPTF --> DECODEF[Decode Frames]:::process',
    '    DECODEF --> SLICEF[Slice to<br>Client Range]:::process',
    '    SLICEF --> SERVE[Stream to<br>Client]:::success',
    '    PLAINREAD --> SERVE',
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

  mermaid.render('comp-mermaid-svg', diagramSrc).then(function(result) {
    document.getElementById('ac-diagram').innerHTML = result.svg;
    wireUpInteractivity();
  });

  var nodeInfo = {
    PUT: {
      title: 'PutObject or Multipart Complete',
      badge: 'entry', badgeText: 'entry point',
      body: '<p>Both write paths encode, and both encode once. A single PUT encodes ahead of the failover loop, so a retry replays already-encoded bytes and rebuilds only the encryption layer.</p><p>A multipart upload is encoded when its parts are assembled, not part by part as they arrive. Its chunk layout therefore owes nothing to the part sizes the client chose, which matters because those are arbitrary: a client picking 8 MiB parts and one picking 500 MiB parts produce identically seekable objects.</p><p><a href="../write-path/">Write path diagram &rarr;</a></p>'
    },
    MINSIZE: {
      title: 'Size Floor (min_size)',
      badge: 'filter', badgeText: 'threshold',
      body: '<p>Objects below <code>compression.min_size</code> (default 4096) are stored verbatim.</p><p>A seek table and per-frame headers are a fixed cost per object. Below some size that cost exceeds anything the encoding could save, so the floor avoids paying it for no return.</p><p>This is the only skip that is free: it is answerable from the size alone, so an object below it costs no encode at all.</p><p class="ac-metric">Metric: s3o_compression_skipped_total{reason="min_size"}</p>'
    },
    ENCODE: {
      title: 'Encode: zstd Frames',
      badge: 'process', badgeText: 'encoding',
      body: '<p>The buffered body is encoded into a second buffer as one independently decodable zstd frame per <code>chunk_size</code> of input (default 1 MiB, range 16 KiB to 64 MiB).</p><p>Both buffers are held until the upload settles: the encoded copy has to replay on every failover attempt, and the plaintext is what it was encoded from. Bodies above 32 MiB spill to a self-unlinking tempfile rather than being held on the heap.</p><p>The level is a name rather than a number - <code>fastest</code>, <code>default</code>, <code>better</code>, <code>best</code> - because zstd collapses its numeric 1-19 range into four buckets, and a numeric setting would let an operator express a distinction the encoder discards.</p><p><a href="../../docs/compression/">Compression reference &rarr;</a></p>'
    },
    SEEKTBL: {
      title: 'Seek Table',
      badge: 'process', badgeText: 'index',
      body: '<p>The frame index is written into a trailing <b>skippable</b> frame, which is what makes the result a valid Zstandard stream that any conforming decoder can read.</p><p>That matters for recovery: <code>zstd -d</code> decodes an object this orchestrator wrote without knowing anything about the seek table, so the bytes on a backend are readable without this software.</p><p>It is also what identifies an encoding to reconcile. Frame magic cannot: a <code>.zst</code> file a client uploaded has the same magic, and decoding one of those on read would return bytes the client never sent. A plain zstd encoder writes no seek table.</p>'
    },
    RATIO: {
      title: 'Ratio Floor (min_ratio)',
      badge: 'filter', badgeText: 'threshold',
      body: '<p>An encoding larger than <code>min_ratio</code> of the original size (default 0.95) is discarded and the object stored as the client sent it.</p><p>This is about entropy rather than size: random data compresses to a ratio of exactly 1.000, so already-compressed content, media and archives gain nothing at any size.</p><p>The decision is made by encoding and measuring, not by sampling the first chunk. Entropy is not uniform across an object, so a sample can be wrong in the direction that costs bytes for the whole life of that object. Encoding something incompressible is the encoder\'s cheapest case: zstd stores blocks it cannot shrink raw, about five times faster than compressing data it can.</p><p class="ac-metric">Metric: s3o_compression_skipped_total{reason="min_ratio"}</p>'
    },
    DISCARD: {
      title: 'Discard Encoding',
      badge: 'reject', badgeText: 'not worth storing',
      body: '<p>The encoded buffer is released and the plan goes back to describing the plaintext it was made from.</p><p>Storing an encoding that did not shrink buys nothing and costs a decode on every later read of that object, which is the trade the floor exists to refuse.</p><p>On a fleet of media or archives most objects land here. That is a healthy run, not a broken one, which is why a rewrite pass counts these as skipped rather than failed.</p>'
    },
    ADMIT: {
      title: 'Admit on Encoded Size',
      badge: 'filter', badgeText: 'quota check',
      body: '<p>Placement and usage admission run against the bytes that will actually occupy the backend.</p><p>This is the one asymmetry with encryption. An envelope is a header plus a tag per chunk, a fixed function of the size, so an encrypted write is admitted before it starts. An encoder only reports its output size once it has run, so a compressed write cannot be - and admitting it on the logical size would turn away a write that fits.</p><p>That is why the ratio and size floors are evaluated before this point and the quota check after.</p>'
    },
    ENCRYPT: {
      title: 'Encryption Enabled?',
      badge: 'decision', badgeText: 'layer check',
      body: '<p>Compression runs before encryption, in that order only, because ciphertext does not compress.</p><p>That ordering makes the compressed stream the encryptor\'s input, which is what <code>plaintext_size</code> records - the pre-encryption size, not the object the client wrote. The client\'s own size lives in <code>logical_size</code>.</p><p><a href="../encryption/">Encryption flow diagram &rarr;</a></p>'
    },
    ENVELOPE: {
      title: 'Encrypt the Encoded Stream',
      badge: 'process', badgeText: 'envelope',
      body: '<p>The encoded stream is wrapped in an AES-256-GCM envelope exactly as an unencoded body would be: a fresh data key per object, a 32-byte header, and a nonce plus auth tag per encryption chunk.</p><p>The two chunk sizes are independent. Compression frames are <code>compression.chunk_size</code> of logical input; encryption chunks are <code>encryption.chunk_size</code> of the compressed stream.</p>'
    },
    VERBATIM: {
      title: 'Store Verbatim',
      badge: 'process', badgeText: 'unencoded',
      body: '<p>The object is stored as the client sent it, and its row carries no algorithm.</p><p>Nothing on the read path distinguishes an object that was too small from one that would not shrink, because nothing needs to: both are simply bytes the ledger describes as verbatim.</p><p>Every row that predates the feature is in exactly this state, which is why enabling compression needs no backfill.</p>'
    },
    UPLOAD: {
      title: 'Upload to Backend',
      badge: 'storage', badgeText: 'S3 API call',
      body: '<p>A single PUT of whatever the layers produced: encoded bytes, an envelope, or the client\'s own bytes.</p><p>The ingress charged is the size actually sent, which is the same figure the ledger row commits, so the storage and bandwidth counters describe the object identically.</p>'
    },
    ROW: {
      title: 'Record the Stored Form',
      badge: 'success', badgeText: 'committed',
      body: '<p>Four nullable columns on <code>object_locations</code> describe the encoding: <code>compression_algorithm</code>, <code>compression_level</code>, <code>compression_format_version</code> and <code>logical_size</code>.</p><p><code>size_bytes</code> counts what the backend holds, so <code>logical_size</code> is the only place the client-visible size survives. Quota and the usage counters both charge the stored size; <code>logical_size</code> exists to answer the client and never to charge the operator.</p><p>The same columns live on <code>pending_objects</code>, because an intent records what was written before the commit. A reaper promoting one has to carry the representation forward, or the promoted row describes bytes that cannot be read.</p><p><a href="../database-schema/">Database schema &rarr;</a></p>'
    },
    GETR: {
      title: 'GetObject',
      badge: 'entry', badgeText: 'entry point',
      body: '<p>The read path decides on the ledger row, not on the bytes. Every copy of a key agrees on whether it is compressed, so whichever copy wins failover takes the same branch.</p><p><a href="../read-path/">Read path diagram &rarr;</a></p>'
    },
    ISCOMP: {
      title: 'Row Carries an Algorithm?',
      badge: 'decision', badgeText: 'stored form check',
      body: '<p>An empty <code>compression_algorithm</code> means the bytes are stored verbatim. Because that single column decides it, no separate boolean can drift out of step with the bytes it describes.</p><p>A copy recorded without its algorithm is not a degraded copy but an unreadable one: the bytes are chunked zstd and the row says they are not, so the read path would serve them raw at the wrong size.</p>'
    },
    RDTABLE: {
      title: 'Fetch Seek Table',
      badge: 'storage', badgeText: 'S3 API call',
      body: '<p>The table lives at the end of the object, so the first fetch a compressed read makes is a ranged GET of its tail rather than of byte zero.</p><p>The size handed to the decoder is the compressed stream\'s size, not the stored size: for an encrypted copy the stored bytes are its ciphertext, so the figure comes from <code>plaintext_size</code> rather than <code>size_bytes</code>.</p><p>Object metadata - content type, ETag, user metadata - is captured from this response rather than paid for with a separate HEAD.</p>'
    },
    MAPFRAME: {
      title: 'Map Range to Covering Frames',
      badge: 'process', badgeText: 'offset maths',
      body: '<p>The client\'s <code>Range</code> is in logical coordinates, against the size the client wrote. The seek table turns that into the set of frames covering it.</p><p>This is the payoff for chunking. A range costs the frames it covers rather than the object, so the cost of a partial read is proportional to the bytes asked for.</p>'
    },
    FETCHF: {
      title: 'Ranged GET per Frame',
      badge: 'storage', badgeText: 'S3 API call',
      body: '<p>One ranged backend GET per frame the read touches.</p><p>Each is charged its own API call and its own egress, on the bytes that actually left the backend. Charging once per client request would under-report all but the first.</p><p>Read amplification is exactly the sum of these against what the client was served. If it climbs toward the average object size, reads have regressed to fetching whole objects and decoding them - a change nothing else would surface except the backend bill.</p><p class="ac-metric">Metrics: s3o_compression_fetched_bytes_total, s3o_compression_served_bytes_total</p>'
    },
    DECR: {
      title: 'Copy Encrypted?',
      badge: 'decision', badgeText: 'layer check',
      body: '<p>Read order is the reverse of write order: decrypt, then decompress, then slice.</p><p>Because the compressed stream is the encryptor\'s plaintext domain, a compressed-domain range translates into a ciphertext range through the same chunk arithmetic the uncompressed path uses, with no new maths.</p>'
    },
    DECRYPTF: {
      title: 'Decrypt Ciphertext Chunks',
      badge: 'process', badgeText: 'range decryption',
      body: '<p>Whole ciphertext chunks are fetched because each carries its own GCM auth tag, so the bytes crossing the backend link exceed the frame requested - and that is what the egress charge counts.</p><p class="ac-metric">Metric: s3o_encryption_ops_total{operation="decrypt_range"}</p>'
    },
    DECODEF: {
      title: 'Decode Frames',
      badge: 'process', badgeText: 'decompression',
      body: '<p>Frames are decoded as the client reads, not up front. Each is independently decodable, which is what makes an entry point every <code>chunk_size</code> possible instead of only at byte zero.</p><p>A decode failure means bytes already stored cannot be read back, which is a different severity from an encode failure costing one write. It deserves an alert on any value.</p><p class="ac-metric">Metric: s3o_compression_errors_total{operation="decode"}</p>'
    },
    SLICEF: {
      title: 'Slice to Client Range',
      badge: 'process', badgeText: 'range slice',
      body: '<p>The decoded reader is seeked to the range start and limited to its length, and <code>Content-Range</code> is reported over the logical size.</p><p>An unsatisfiable range is served as the whole object, which is what the uncompressed path does with one it cannot translate.</p>'
    },
    PLAINREAD: {
      title: 'Whole-object GET',
      badge: 'storage', badgeText: 'S3 API call',
      body: '<p>A verbatim copy is read the way it always was: one GET, with any client range passed through to the backend unchanged, since the stored bytes are already in the coordinates the client used.</p>'
    },
    SERVE: {
      title: 'Stream to Client',
      badge: 'success', badgeText: 'success',
      body: '<p>The client receives the bytes it wrote, at the size it wrote them, with the ETag and content hash of the logical object. Nothing about the stored form is visible from the S3 API.</p><p>That invariant is what the whole feature is held to: <code>content_hash</code> covers the logical bytes, so the scrubber undoes the stored form before hashing rather than hashing what the backend holds.</p><p><a href="../background-services/">Background services &rarr;</a></p>'
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
| <span style="color:#c4a35a">**Amber**</span> | Threshold / quota filtering |
| <span style="color:#2a9d73">**Green border**</span> | Decision / branch |
| <span style="color:#5ec9a0">**Teal**</span> | Processing step |
| <span style="color:#4aaa8a">**Teal**</span> | Storage / S3 |
| <span style="color:#34b882">**Green**</span> | Success |
| <span style="color:#d4a0a0">**Red**</span> | Discarded encoding |

## See also

- [Compression reference](../../docs/compression/) - configuration, operations and what to watch
- [Write path](../write-path/) and [read path](../read-path/) - where the codec sits in each
- [Encryption flow](../encryption/) - the layer compression composes with
