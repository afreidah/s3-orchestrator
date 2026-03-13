---
title: "Database Schema"
linkTitle: "Database Schema"
weight: 7
---

Entity-relationship diagram of the PostgreSQL metadata store. **Hover over any table** for column details and usage context.

<style>
  #ac-diagram { margin: 1rem 0; }
  #ac-diagram svg { display: block; width: 100%; height: auto; }
  #ac-tooltip {
    position: fixed; z-index: 9999;
    max-width: 420px; padding: 0.7rem 0.85rem;
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
  .ac-badge-core { background: #1f6feb22; color: #58a6ff; border: 1px solid #58a6ff55; }
  .ac-badge-multipart { background: #8957e522; color: #bc8cff; border: 1px solid #bc8cff55; }
  .ac-badge-usage { background: #9e6a0322; color: #d29922; border: 1px solid #d2992255; }
  .ac-badge-cleanup { background: #da363322; color: #f85149; border: 1px solid #f8514955; }
  #ac-tooltip p { font-size: 0.75rem; line-height: 1.4; color: #c9d1d9; margin-bottom: 0.35rem; }
  #ac-tooltip code { background: #21262d; padding: 1px 4px; border-radius: 3px; font-size: 0.7rem; color: #79c0ff; }
  #ac-tooltip .ac-metric { color: #d2a8ff; font-style: italic; font-size: 0.7rem; }
  #ac-tooltip table.ac-cols { width: 100%; border-collapse: collapse; margin: 0.3rem 0; font-size: 0.68rem; }
  #ac-tooltip table.ac-cols th { text-align: left; color: #8b949e; font-weight: 600; padding: 2px 6px 2px 0; border-bottom: 1px solid #30363d; }
  #ac-tooltip table.ac-cols td { padding: 1px 6px 1px 0; color: #c9d1d9; }
  #ac-tooltip table.ac-cols td:first-child { color: #79c0ff; font-family: monospace; }
  #ac-tooltip table.ac-cols td.pk { color: #f0883e; }
  #ac-tooltip table.ac-cols td.fk { color: #bc8cff; }
  #ac-tooltip .ac-idx { color: #8b949e; font-size: 0.66rem; margin-top: 0.2rem; }
  #ac-diagram .entityBox { cursor: pointer; transition: opacity 0.15s, filter 0.15s; }
  #ac-diagram .er { transition: opacity 0.15s; }
</style>

<div id="ac-diagram"></div>
<div id="ac-tooltip"></div>

<script src="https://cdn.jsdelivr.net/npm/mermaid@11/dist/mermaid.min.js"></script>
<script>
(function() {
  var diagramSrc = [
    'erDiagram',
    '    backend_quotas {',
    '        TEXT backend_name PK',
    '        BIGINT bytes_used',
    '        BIGINT bytes_limit',
    '        BIGINT orphan_bytes',
    '        TIMESTAMPTZ updated_at',
    '    }',
    '',
    '    object_locations {',
    '        TEXT object_key PK',
    '        TEXT backend_name "PK, FK"',
    '        BIGINT size_bytes',
    '        BOOLEAN encrypted',
    '        BYTEA encryption_key',
    '        TEXT key_id',
    '        BIGINT plaintext_size',
    '        TIMESTAMPTZ created_at',
    '    }',
    '',
    '    multipart_uploads {',
    '        TEXT upload_id PK',
    '        TEXT object_key',
    '        TEXT backend_name FK',
    '        TEXT content_type',
    '        JSONB metadata',
    '        TIMESTAMPTZ created_at',
    '    }',
    '',
    '    multipart_parts {',
    '        TEXT upload_id "PK, FK"',
    '        INT part_number PK',
    '        TEXT etag',
    '        BIGINT size_bytes',
    '        BOOLEAN encrypted',
    '        BYTEA encryption_key',
    '        TEXT key_id',
    '        BIGINT plaintext_size',
    '        TIMESTAMPTZ created_at',
    '    }',
    '',
    '    backend_usage {',
    '        TEXT backend_name "PK, FK"',
    '        TEXT period PK',
    '        BIGINT api_requests',
    '        BIGINT egress_bytes',
    '        BIGINT ingress_bytes',
    '        TIMESTAMPTZ updated_at',
    '    }',
    '',
    '    cleanup_queue {',
    '        BIGSERIAL id PK',
    '        TEXT backend_name FK',
    '        TEXT object_key',
    '        TEXT reason',
    '        BIGINT size_bytes',
    '        TIMESTAMPTZ created_at',
    '        TIMESTAMPTZ next_retry',
    '        INT attempts',
    '        TEXT last_error',
    '    }',
    '',
    '    backend_quotas ||--o{ object_locations : "tracks objects"',
    '    backend_quotas ||--o{ multipart_uploads : "tracks uploads"',
    '    backend_quotas ||--o{ backend_usage : "monthly usage"',
    '    backend_quotas ||--o{ cleanup_queue : "pending deletes"',
    '    multipart_uploads ||--o{ multipart_parts : "upload parts"'
  ].join('\n');

  mermaid.initialize({
    startOnLoad: false, theme: 'dark', fontSize: 16,
    er: { useMaxWidth: false, minEntityWidth: 160, entityPadding: 18, fontSize: 16 }
  });

  mermaid.render('db-mermaid-svg', diagramSrc).then(function(result) {
    document.getElementById('ac-diagram').innerHTML = result.svg;
    wireUpInteractivity();
  });

  var nodeInfo = {
    backend_quotas: {
      title: 'backend_quotas',
      badge: 'core', badgeText: 'core table',
      body: '<p>Central registry of S3 backends. Every other table references this via <code>backend_name</code> foreign key. Created at startup from config; quota limits synced on each boot via <code>UpsertQuotaLimit</code>.</p>' +
        '<table class="ac-cols"><tr><th>Column</th><th>Type</th><th>Notes</th></tr>' +
        '<tr><td class="pk">backend_name</td><td>TEXT</td><td>PRIMARY KEY</td></tr>' +
        '<tr><td>bytes_used</td><td>BIGINT</td><td>Running total, atomically incremented/decremented</td></tr>' +
        '<tr><td>bytes_limit</td><td>BIGINT</td><td>Quota cap (0 = unlimited)</td></tr>' +
        '<tr><td>orphan_bytes</td><td>BIGINT</td><td>Bytes freed logically but not yet deleted from backend</td></tr>' +
        '<tr><td>updated_at</td><td>TIMESTAMPTZ</td><td>Last modification time</td></tr></table>' +
        '<p class="ac-idx"><b>Indexes:</b> PK on backend_name</p>' +
        '<p>Used by: <a href="../write-path/">write path</a> (backend selection), quota enforcement, <a href="../background-services/">rebalancer</a>, dashboard stats.</p>' +
        '<p class="ac-metric">Key queries: GetLeastUtilizedBackend, GetBackendAvailableSpace, IncrementQuota, DecrementQuota, IncrementOrphanBytes</p>'
    },
    object_locations: {
      title: 'object_locations',
      badge: 'core', badgeText: 'core table',
      body: '<p>Maps every stored object to its backend(s). Composite primary key <code>(object_key, backend_name)</code> supports replication &mdash; one object can exist on multiple backends. Added encryption columns track envelope-encrypted objects.</p>' +
        '<table class="ac-cols"><tr><th>Column</th><th>Type</th><th>Notes</th></tr>' +
        '<tr><td class="pk">object_key</td><td>TEXT</td><td>PK (composite)</td></tr>' +
        '<tr><td class="fk">backend_name</td><td>TEXT</td><td>PK + FK &rarr; backend_quotas</td></tr>' +
        '<tr><td>size_bytes</td><td>BIGINT</td><td>Ciphertext size if encrypted</td></tr>' +
        '<tr><td>encrypted</td><td>BOOLEAN</td><td>Envelope encryption flag</td></tr>' +
        '<tr><td>encryption_key</td><td>BYTEA</td><td>Packed nonce + wrapped DEK</td></tr>' +
        '<tr><td>key_id</td><td>TEXT</td><td>KMS/Vault key version identifier</td></tr>' +
        '<tr><td>plaintext_size</td><td>BIGINT</td><td>Original size before encryption</td></tr>' +
        '<tr><td>created_at</td><td>TIMESTAMPTZ</td><td>Insert timestamp</td></tr></table>' +
        '<p class="ac-idx"><b>Indexes:</b> PK (object_key, backend_name) &bull; idx_object_locations_backend (backend_name) &bull; idx_object_locations_key_pattern (object_key text_pattern_ops) &bull; idx_object_locations_created (created_at)</p>' +
        '<p>Used by: <a href="../write-path/">write path</a> (RecordObject), <a href="../read-path/">read path</a> (GetAllObjectLocations), <a href="../background-services/">replicator</a> (GetUnderReplicatedObjects), directory tree listing, <a href="../encryption/">key rotation</a>.</p>' +
        '<p class="ac-metric">Key queries: InsertObjectLocation, ListObjectsByPrefix, GetDirectoryStats, GetUnderReplicatedObjects, BackendObjectStats</p>'
    },
    multipart_uploads: {
      title: 'multipart_uploads',
      badge: 'multipart', badgeText: 'multipart',
      body: '<p>Tracks in-progress multipart uploads. Each upload is pinned to a single backend at initiation time. The <code>metadata</code> JSONB column stores user-provided <code>x-amz-meta-*</code> headers for replay at completion.</p>' +
        '<table class="ac-cols"><tr><th>Column</th><th>Type</th><th>Notes</th></tr>' +
        '<tr><td class="pk">upload_id</td><td>TEXT</td><td>PRIMARY KEY (UUID)</td></tr>' +
        '<tr><td>object_key</td><td>TEXT</td><td>Target object key</td></tr>' +
        '<tr><td class="fk">backend_name</td><td>TEXT</td><td>FK &rarr; backend_quotas</td></tr>' +
        '<tr><td>content_type</td><td>TEXT</td><td>MIME type from initiation</td></tr>' +
        '<tr><td>metadata</td><td>JSONB</td><td>User metadata (x-amz-meta-*)</td></tr>' +
        '<tr><td>created_at</td><td>TIMESTAMPTZ</td><td>Upload initiation time</td></tr></table>' +
        '<p class="ac-idx"><b>Indexes:</b> PK on upload_id &bull; idx_multipart_uploads_created (created_at) &bull; idx_multipart_uploads_key_pattern (object_key text_pattern_ops)</p>' +
        '<p>Used by: CreateMultipartUpload, UploadPart, CompleteMultipartUpload, AbortMultipartUpload, <a href="../background-services/">stale upload cleanup</a> (GetStaleMultipartUploads), quota available-space calculation (inflight parts JOIN).</p>' +
        '<p class="ac-metric">Key queries: CreateMultipartUpload, GetMultipartUpload, GetStaleMultipartUploads, ListMultipartUploadsByPrefix</p>'
    },
    multipart_parts: {
      title: 'multipart_parts',
      badge: 'multipart', badgeText: 'multipart',
      body: '<p>Stores individual parts of a multipart upload. Foreign key to <code>multipart_uploads</code> with <code>ON DELETE CASCADE</code> &mdash; aborting an upload deletes all its parts automatically. Supports upsert for part re-upload.</p>' +
        '<table class="ac-cols"><tr><th>Column</th><th>Type</th><th>Notes</th></tr>' +
        '<tr><td class="fk">upload_id</td><td>TEXT</td><td>PK + FK &rarr; multipart_uploads (CASCADE)</td></tr>' +
        '<tr><td class="pk">part_number</td><td>INT</td><td>PK (composite), 1-10000</td></tr>' +
        '<tr><td>etag</td><td>TEXT</td><td>MD5 hash of part data</td></tr>' +
        '<tr><td>size_bytes</td><td>BIGINT</td><td>Part size (ciphertext if encrypted)</td></tr>' +
        '<tr><td>encrypted</td><td>BOOLEAN</td><td>Envelope encryption flag</td></tr>' +
        '<tr><td>encryption_key</td><td>BYTEA</td><td>Per-part nonce + wrapped DEK</td></tr>' +
        '<tr><td>key_id</td><td>TEXT</td><td>KMS/Vault key version</td></tr>' +
        '<tr><td>plaintext_size</td><td>BIGINT</td><td>Original part size</td></tr>' +
        '<tr><td>created_at</td><td>TIMESTAMPTZ</td><td>Part upload time</td></tr></table>' +
        '<p class="ac-idx"><b>Indexes:</b> PK (upload_id, part_number)</p>' +
        '<p>Used by: UploadPart (UpsertPart), CompleteMultipartUpload (GetParts &mdash; ordered by part_number), quota calculation (SUM of inflight part sizes JOINed to uploads).</p>' +
        '<p class="ac-metric">Key queries: UpsertPart, GetParts</p>'
    },
    backend_usage: {
      title: 'backend_usage',
      badge: 'usage', badgeText: 'usage tracking',
      body: '<p>Monthly rolling usage counters per backend. Keyed by <code>(backend_name, period)</code> where period is <code>YYYY-MM</code> format. In-memory atomic counters are flushed to this table periodically via <code>FlushUsageDeltas</code> (upsert with atomic ADD).</p>' +
        '<table class="ac-cols"><tr><th>Column</th><th>Type</th><th>Notes</th></tr>' +
        '<tr><td class="fk">backend_name</td><td>TEXT</td><td>PK + FK &rarr; backend_quotas</td></tr>' +
        '<tr><td class="pk">period</td><td>TEXT</td><td>PK, e.g. "2026-03"</td></tr>' +
        '<tr><td>api_requests</td><td>BIGINT</td><td>S3 API call count</td></tr>' +
        '<tr><td>egress_bytes</td><td>BIGINT</td><td>Bytes downloaded from backend</td></tr>' +
        '<tr><td>ingress_bytes</td><td>BIGINT</td><td>Bytes uploaded to backend</td></tr>' +
        '<tr><td>updated_at</td><td>TIMESTAMPTZ</td><td>Last flush time</td></tr></table>' +
        '<p class="ac-idx"><b>Indexes:</b> PK (backend_name, period)</p>' +
        '<p>Used by: <a href="../write-path/">write path</a> eligibility filter (BackendsWithinLimits baseline), usage flush <a href="../background-services/">background service</a>, dashboard monthly usage chart.</p>' +
        '<p class="ac-metric">Key queries: FlushUsageDeltas (INSERT ON CONFLICT DO UPDATE), GetUsageForPeriod</p>'
    },
    cleanup_queue: {
      title: 'cleanup_queue',
      badge: 'cleanup', badgeText: 'cleanup queue',
      body: '<p>Retry queue for failed backend object deletions. When a delete fails (network error, backend down), the orphaned object is enqueued here with exponential backoff (1 min &rarr; 24 hours, max 10 attempts). Prevents storage leaks from transient failures.</p>' +
        '<table class="ac-cols"><tr><th>Column</th><th>Type</th><th>Notes</th></tr>' +
        '<tr><td class="pk">id</td><td>BIGSERIAL</td><td>PRIMARY KEY (auto-increment)</td></tr>' +
        '<tr><td class="fk">backend_name</td><td>TEXT</td><td>FK &rarr; backend_quotas</td></tr>' +
        '<tr><td>object_key</td><td>TEXT</td><td>S3 key to delete</td></tr>' +
        '<tr><td>reason</td><td>TEXT</td><td>Why cleanup is needed</td></tr>' +
        '<tr><td>size_bytes</td><td>BIGINT</td><td>Object size (for orphan_bytes tracking)</td></tr>' +
        '<tr><td>created_at</td><td>TIMESTAMPTZ</td><td>Enqueue time</td></tr>' +
        '<tr><td>next_retry</td><td>TIMESTAMPTZ</td><td>Earliest retry time</td></tr>' +
        '<tr><td>attempts</td><td>INT</td><td>Retry count (max 10)</td></tr>' +
        '<tr><td>last_error</td><td>TEXT</td><td>Most recent error message</td></tr></table>' +
        '<p class="ac-idx"><b>Indexes:</b> PK on id &bull; idx_cleanup_queue_next_retry (next_retry) WHERE attempts &lt; 10 (partial index)</p>' +
        '<p>Used by: enqueueCleanup() at all failure sites (PutObject, DeleteObject, multipart ops, <a href="../background-services/">rebalancer</a>, replicator), <a href="../background-services/">cleanupQueueService</a> background worker (runs every 1 min).</p>' +
        '<p class="ac-metric">Key queries: EnqueueCleanup, GetPendingCleanups, UpdateCleanupRetry, CountPendingCleanups</p>' +
        '<p class="ac-metric">Metrics: cleanup_queue_enqueued_total, cleanup_queue_processed_total, cleanup_queue_depth</p>'
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
  }

  function wireUpInteractivity() {
    var svg = document.querySelector('#ac-diagram svg');
    if (!svg) return;

    // Mermaid ER diagrams render entities as g.entity elements with an id
    // matching the entity name or containing it. We search for all entity
    // groups and wire hover events.
    var entities = svg.querySelectorAll('g[id]');
    entities.forEach(function(g) {
      var gId = g.getAttribute('id') || '';
      // Mermaid ER diagram entity IDs follow the pattern: entity-TABLE_NAME-N
      // or just the table name directly. Try to extract the table name.
      var tableName = null;
      var tableNames = ['backend_quotas', 'object_locations', 'multipart_uploads', 'multipart_parts', 'backend_usage', 'cleanup_queue'];
      for (var i = 0; i < tableNames.length; i++) {
        if (gId.indexOf(tableNames[i]) !== -1) {
          tableName = tableNames[i];
          break;
        }
      }
      if (!tableName) return;

      g.style.cursor = 'pointer';
      g.addEventListener('mouseenter', function() {
        hoveringNode = true; clearTimeout(hideTimer);
        showInfo(tableName);
      });
      g.addEventListener('mouseleave', function() {
        hoveringNode = false;
        hideTimer = setTimeout(function() { if (!hoveringNode && !hoveringTooltip) clearInfo(); }, 100);
      });
    });
  }
})();
</script>

## Legend

| Symbol | Meaning |
|--------|---------|
| <span style="color:#58a6ff">**PK**</span> | Primary key column |
| <span style="color:#bc8cff">**FK**</span> | Foreign key reference |
| `\|\|--o{` | One-to-many relationship |
| <span style="color:#f0883e">**BIGSERIAL**</span> | Auto-incrementing surrogate key |
| <span style="color:#79c0ff">**text_pattern_ops**</span> | B-tree index optimized for LIKE prefix queries |

### Table Summary

| Table | Purpose | Primary Key |
|-------|---------|-------------|
| **backend_quotas** | Backend registry with storage quota tracking | `backend_name` |
| **object_locations** | Maps objects to backends (supports replication) | `(object_key, backend_name)` |
| **multipart_uploads** | In-progress multipart upload state | `upload_id` |
| **multipart_parts** | Individual parts within a multipart upload | `(upload_id, part_number)` |
| **backend_usage** | Monthly API/bandwidth counters per backend | `(backend_name, period)` |
| **cleanup_queue** | Retry queue for failed orphan deletions | `id` (auto-increment) |

### Schema Migrations

| Migration | Description |
|-----------|-------------|
| `00001_init_schema` | All six tables, indexes, and foreign keys |
| `00002_multipart_metadata` | Add `metadata` JSONB column to `multipart_uploads` |
| `00003_add_encryption` | Add encryption columns to `object_locations` and `multipart_parts` |
| `00004_add_orphan_bytes` | Add `orphan_bytes` to `backend_quotas` and `size_bytes` to `cleanup_queue` |
