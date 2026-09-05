---
description: "Interactive entity-relationship diagram of the PostgreSQL metadata store, with column details and usage context on every table."
title: "Database Schema"
linkTitle: "Database Schema"
weight: 8
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
  #ac-tooltip a { color: #34b882; text-decoration: none; }
  #ac-tooltip a:hover { text-decoration: underline; }
  #ac-tooltip h3 { color: #2a9d73; font-size: 0.85rem; margin: 0 0 0.25rem 0; }
  #ac-tooltip .ac-badge {
    display: inline-block; padding: 1px 7px; border-radius: 4px;
    font-size: 0.6rem; font-weight: 600; margin-bottom: 0.4rem; text-transform: uppercase;
  }
  .ac-badge-core { background: #1f6feb22; color: #58a6ff; border: 1px solid #58a6ff55; }
  .ac-badge-multipart { background: #8957e522; color: #bc8cff; border: 1px solid #bc8cff55; }
  .ac-badge-usage { background: #9e6a0322; color: #d29922; border: 1px solid #d2992255; }
  .ac-badge-cleanup { background: #da363322; color: #f85149; border: 1px solid #f8514955; }
  #ac-tooltip p { font-size: 0.75rem; line-height: 1.4; color: #c9d1d9; margin-bottom: 0.35rem; }
  #ac-tooltip code { background: #21262d; padding: 1px 4px; border-radius: 3px; font-size: 0.7rem; color: #4aaa8a; }
  #ac-tooltip .ac-metric { color: #a7d5c1; font-style: italic; font-size: 0.7rem; }
  #ac-tooltip table.ac-cols { width: 100%; border-collapse: collapse; margin: 0.3rem 0; font-size: 0.68rem; }
  #ac-tooltip table.ac-cols th { text-align: left; color: #8b949e; font-weight: 600; padding: 2px 6px 2px 0; border-bottom: 1px solid #30363d; }
  #ac-tooltip table.ac-cols td { padding: 1px 6px 1px 0; color: #c9d1d9; }
  #ac-tooltip table.ac-cols td:first-child { color: #4aaa8a; font-family: monospace; }
  #ac-tooltip table.ac-cols td.pk { color: #f0883e; }
  #ac-tooltip table.ac-cols td.fk { color: #bc8cff; }
  #ac-tooltip .ac-idx { color: #8b949e; font-size: 0.66rem; margin-top: 0.2rem; }
  #ac-diagram .entityBox { cursor: pointer; transition: opacity 0.15s, filter 0.15s; }
  #ac-diagram .er { transition: opacity 0.15s; }
</style>

<div id="ac-diagram"></div>
<div id="ac-tooltip"></div>

<script src="https://cdn.jsdelivr.net/npm/mermaid@11.8.0/dist/mermaid.min.js"></script>
<script>
(function() {
  var diagramSrc = [
    'erDiagram',
    '    backend_quotas {',
    '        TEXT backend_name PK',
    '        BIGINT bytes_limit',
    '        BIGINT orphan_bytes',
    '        TIMESTAMPTZ updated_at',
    '    }',
    '',
    '    backend_quota_stripes {',
    '        TEXT backend_name "PK, FK"',
    '        SMALLINT stripe_id PK',
    '        BIGINT bytes_used',
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
    '        TEXT content_hash',
    '        TEXT compression_algorithm',
    '        TEXT compression_level',
    '        SMALLINT compression_format_version',
    '        BIGINT logical_size',
    '        BIGINT compression_probe_size',
    '        TEXT compression_probe_level',
    '        BOOLEAN managed',
    '        TIMESTAMPTZ last_scrubbed_at',
    '        TIMESTAMPTZ created_at',
    '    }',
    '',
    '    object_tags {',
    '        TEXT object_key PK',
    '        TEXT tag_key PK',
    '        TEXT tag_value',
    '    }',
    '',
    '    multipart_uploads {',
    '        TEXT upload_id PK',
    '        TEXT object_key',
    '        TEXT backend_name FK',
    '        TEXT content_type',
    '        JSONB metadata',
    '        BYTEA encryption_key',
    '        TEXT key_id',
    '        TEXT tagging',
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
    '    backend_request_usage {',
    '        TEXT backend_name "PK, FK"',
    '        TEXT period PK',
    '        TEXT pool PK',
    '        BIGINT requests',
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
    '        TIMESTAMPTZ claimed_at',
    '        TEXT claimed_by',
    '    }',
    '',
    '    cleanup_dlq {',
    '        BIGSERIAL id PK',
    '        BIGINT original_id',
    '        TEXT backend_name FK',
    '        TEXT object_key',
    '        TEXT reason',
    '        BIGINT size_bytes',
    '        INT attempts',
    '        TIMESTAMPTZ first_enqueued_at',
    '        TIMESTAMPTZ moved_at',
    '        TEXT last_error',
    '    }',
    '',
    '    pending_objects {',
    '        TEXT intent_id PK',
    '        TEXT object_key',
    '        TEXT backend_name FK',
    '        BIGINT size_bytes',
    '        BOOLEAN encrypted',
    '        BYTEA encryption_key',
    '        TEXT key_id',
    '        BIGINT plaintext_size',
    '        TEXT content_hash',
    '        TEXT compression_algorithm',
    '        TEXT compression_level',
    '        SMALLINT compression_format_version',
    '        BIGINT logical_size',
    '        TIMESTAMPTZ created_at',
    '    }',
    '',
    '    notification_outbox {',
    '        BIGSERIAL id PK',
    '        TEXT event_type',
    '        JSONB payload',
    '        TEXT endpoint_url',
    '        TIMESTAMPTZ created_at',
    '        TIMESTAMPTZ next_retry',
    '        INT attempts',
    '        TEXT last_error',
    '    }',
    '',
    '    backend_quotas ||--o{ backend_quota_stripes : "striped byte total"',
    '    backend_quotas ||--o{ object_locations : "tracks objects"',
    '    backend_quotas ||--o{ multipart_uploads : "tracks uploads"',
    '    backend_quotas ||--o{ backend_usage : "monthly usage"',
    '    backend_quotas ||--o{ backend_request_usage : "monthly usage per budget pool"',
    '    backend_quotas ||--o{ cleanup_queue : "pending deletes"',
    '    backend_quotas ||--o{ cleanup_dlq : "exhausted orphans"',
    '    backend_quotas ||--o{ pending_objects : "in-flight intents"',
    '    cleanup_queue ||--o| cleanup_dlq : "graduates on retry exhaustion"',
    '    multipart_uploads ||--o{ multipart_parts : "upload parts"',
    '    object_locations ||--o{ object_tags : "tag set, by object_key only"'
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
        '<tr><td>bytes_limit</td><td>BIGINT</td><td>Quota cap (0 = unlimited)</td></tr>' +
        '<tr><td>orphan_bytes</td><td>BIGINT</td><td>Bytes freed logically but not yet deleted from backend</td></tr>' +
        '<tr><td>updated_at</td><td>TIMESTAMPTZ</td><td>Last modification time</td></tr></table>' +
        '<p class="ac-idx"><b>Indexes:</b> PK on backend_name</p>' +
        '<p>Holds the ceiling and the orphan count. The stored byte total lives in <code>backend_quota_stripes</code>.</p>' +
        '<p>Used by: <a href="../write-path/">write path</a> (backend selection), quota enforcement, <a href="../background-services/">rebalancer</a>, dashboard stats.</p>' +
        '<p class="ac-metric">Key queries: UpsertQuotaLimit, ListBackendQuotaUsage, IncrementOrphanBytes, DecrementOrphanBytes</p>'
    },
    backend_quota_stripes: {
      title: 'backend_quota_stripes',
      badge: 'core', badgeText: 'core table',
      body: '<p>A backend\'s stored byte total, split across rows so concurrent writes do not queue. Row locks are per row, so writers landing on different stripes never wait on one another; the backend holds <code>QuotaStripeCount</code> (16) of them.</p>' +
        '<table class="ac-cols"><tr><th>Column</th><th>Type</th><th>Notes</th></tr>' +
        '<tr><td class="pk">backend_name</td><td>TEXT</td><td>PRIMARY KEY, FK to backend_quotas</td></tr>' +
        '<tr><td class="pk">stripe_id</td><td>SMALLINT</td><td>PRIMARY KEY; chosen from the object key, so a charge and the credit reversing it meet on one row</td></tr>' +
        '<tr><td>bytes_used</td><td>BIGINT</td><td>Signed. A stripe carries no meaning alone and may sit negative; the backend total is the SUM, clamped at zero when read</td></tr></table>' +
        '<p class="ac-idx"><b>Indexes:</b> PK on (backend_name, stripe_id)</p>' +
        '<p>Charged inside the same transaction that writes the <code>object_locations</code> rows it summarizes, so it cannot drift from the ledger. Usage reconciliation is an audit rather than a repair the counter depends on.</p>' +
        '<p>Used by: <a href="../write-path/">write path</a> (admission and charging), <a href="../usage-quotas/">quota reporting</a>, <a href="../background-services/">reconcile</a>.</p>' +
        '<p class="ac-metric">Key queries: AdjustQuotaStripe, ListBackendQuotaUsage, GetAllQuotaStats, SetBackendBytesUsed</p>'
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
        '<tr><td>content_hash</td><td>TEXT</td><td>SHA-256 hex digest of plaintext (nullable)</td></tr>' +
        '<tr><td>compression_algorithm</td><td>TEXT</td><td>Codec the stored bytes were encoded with. NULL means stored verbatim, so no separate flag can drift out of step with it (nullable)</td></tr>' +
        '<tr><td>compression_level</td><td>TEXT</td><td>Level the object was written at. Diagnostic only, since decoding does not need it, but a rewrite pass does (nullable)</td></tr>' +
        '<tr><td>compression_format_version</td><td>SMALLINT</td><td>On-disk layout version, so a later change is detectable rather than silently misread (nullable)</td></tr>' +
        '<tr><td>logical_size</td><td>BIGINT</td><td>Size of the object the client wrote. Distinct from plaintext_size: with both features on the stored bytes are ciphertext of compressed data, so plaintext_size is the pre-encryption (compressed) size and this is the original (nullable)</td></tr>' +
        '<tr><td>compression_probe_size</td><td>BIGINT</td><td>What the encoder produced for a copy stored verbatim anyway, so a later pass reaches the same verdict without re-encoding it. Stored as the measurement rather than a declined flag, because min_ratio is applied at query time (nullable)</td></tr>' +
        '<tr><td>compression_probe_level</td><td>TEXT</td><td>Level that probe was measured at, so a measurement taken under different settings is not reused as though it were current (nullable)</td></tr>' +
        '<tr><td>managed</td><td>BOOLEAN</td><td>False for objects reconcile found outside every virtual bucket prefix &mdash; they count toward quota but replication, rebalance, integrity and drain ignore them (default true)</td></tr>' +
        '<tr><td>last_scrubbed_at</td><td>TIMESTAMPTZ</td><td>When the scrubber last verified this copy. NULL means never verified; the scrub queue falls back to created_at so a fresh write sorts behind older unverified data (nullable)</td></tr>' +
        '<tr><td>created_at</td><td>TIMESTAMPTZ</td><td>Insert timestamp</td></tr></table>' +
        '<p class="ac-idx"><b>Indexes:</b> PK (object_key, backend_name) &bull; idx_object_locations_backend (backend_name) &bull; idx_object_locations_key_pattern (object_key text_pattern_ops) &bull; idx_object_locations_created (created_at) &bull; idx_object_locations_key_created (object_key, created_at) &bull; idx_object_locations_backend_key_collate_c (backend_name, object_key COLLATE "C") &bull; idx_object_locations_key_collate_c (object_key COLLATE "C") &bull; idx_object_locations_managed (backend_name) WHERE managed &bull; idx_object_locations_scrub_queue (COALESCE(last_scrubbed_at, created_at), object_key) WHERE content_hash IS NOT NULL AND managed</p>' +
        '<p>Used by: <a href="../write-path/">write path</a> (RecordObject), <a href="../read-path/">read path</a> (GetAllObjectLocations), <a href="../background-services/">replicator</a> (GetUnderReplicatedObjects), directory tree listing, <a href="../encryption/">key rotation</a>, <a href="../compression/">compression</a> (stored-form columns).</p>' +
        '<p class="ac-metric">Key queries: InsertObjectLocation, ListObjectsByPrefix, GetDirectoryStats, GetUnderReplicatedObjects, BackendObjectStats</p>'
    },
    object_tags: {
      title: 'object_tags',
      badge: 'core', badgeText: 'core table',
      body: '<p>S3 object tags: key/value labels attached to an object independently of its data. Keyed by <code>object_key</code> alone, not <code>(object_key, backend_name)</code> &mdash; a tag set describes the object, so per-replica rows would let three copies of a key disagree with nothing to say which wins.</p>' +
        '<p>One row per tag rather than a JSON column on <code>object_locations</code>. Filtering objects by tag is a <code>WHERE tag_key = ? AND tag_value = ?</code>, which needs an index; a JSON blob turns that into a scan over every object. Ten tags per object caps how many rows a key can add.</p>' +
        '<table class="ac-cols"><tr><th>Column</th><th>Type</th><th>Notes</th></tr>' +
        '<tr><td class="pk">object_key</td><td>TEXT</td><td>PRIMARY KEY (composite). Default collation, matching object_locations.object_key, so equality joins need no coercion</td></tr>' +
        '<tr><td class="pk">tag_key</td><td>TEXT</td><td>PRIMARY KEY (composite). Max 128 UTF-16 code units, case sensitive</td></tr>' +
        '<tr><td>tag_value</td><td>TEXT</td><td>Max 256 UTF-16 code units, case sensitive</td></tr></table>' +
        '<p class="ac-idx"><b>Indexes:</b> PK (object_key, tag_key) &bull; idx_object_tags_lookup (tag_key, tag_value)</p>' +
        '<p>No foreign key, because there is no table to point at: <code>object_locations</code> is keyed <code>(object_key, backend_name)</code> and nothing is keyed on object key alone, so <code>ON DELETE CASCADE</code> cannot express this. Core clears these rows instead, at every path that puts a new object at a key or removes the last copy of one. The PK already serves lookup and delete by object key; idx_object_tags_lookup is for the reverse direction.</p>' +
        '<p>Used by: <a href="../tagging/">tagging</a> (PutObjectTagging, GetObjectTagging, DeleteObjectTagging), inline <code>x-amz-tagging</code> on the <a href="../write-path/">write path</a>, CopyObject tagging directive, the <a href="../read-path/">read path</a> for the tagging count on GetObject and HeadObject, lifecycle expiration rules that filter on a tag, admin API and TUI object inspector.</p>' +
        '<p class="ac-metric">Key queries: ReplaceObjectTags, GetObjectTags, CountObjectTags, DeleteObjectTags, ClearTagsForKeys</p>'
    },
    multipart_uploads: {
      title: 'multipart_uploads',
      badge: 'multipart', badgeText: 'multipart',
      body: '<p>Tracks in-progress multipart uploads. Each upload is pinned to a single backend at initiation time. The <code>metadata</code> JSONB column stores user-provided <code>x-amz-meta-*</code> headers for replay at completion, and <code>tagging</code> holds a query-string-encoded tag set until the upload completes.</p>' +
        '<table class="ac-cols"><tr><th>Column</th><th>Type</th><th>Notes</th></tr>' +
        '<tr><td class="pk">upload_id</td><td>TEXT</td><td>PRIMARY KEY (UUID)</td></tr>' +
        '<tr><td>object_key</td><td>TEXT</td><td>Target object key</td></tr>' +
        '<tr><td class="fk">backend_name</td><td>TEXT</td><td>FK &rarr; backend_quotas</td></tr>' +
        '<tr><td>content_type</td><td>TEXT</td><td>MIME type from initiation</td></tr>' +
        '<tr><td>metadata</td><td>JSONB</td><td>User metadata (x-amz-meta-*)</td></tr>' +
        '<tr><td>encryption_key</td><td>BYTEA</td><td>Packed nonce + wrapped DEK (nullable)</td></tr>' +
        '<tr><td>key_id</td><td>TEXT</td><td>KMS/Vault key version identifier (nullable)</td></tr>' +
        '<tr><td>tagging</td><td>TEXT</td><td>Query-string-encoded tag set from CreateMultipartUpload (nullable; NULL means the upload carried no tags)</td></tr>' +
        '<tr><td>created_at</td><td>TIMESTAMPTZ</td><td>Upload initiation time</td></tr></table>' +
        '<p class="ac-idx"><b>Indexes:</b> PK on upload_id &bull; idx_multipart_uploads_created (created_at) &bull; idx_multipart_uploads_key_pattern (object_key text_pattern_ops) &bull; idx_multipart_uploads_backend_name (backend_name)</p>' +
        '<p>Used by: CreateMultipartUpload, UploadPart, CompleteMultipartUpload, AbortMultipartUpload, <a href="../background-services/">stale upload cleanup</a> (GetStaleMultipartUploads), drain (GetMultipartUploadsByBackend), quota available-space calculation (inflight parts JOIN).</p>' +
        '<p class="ac-metric">Key queries: CreateMultipartUpload, GetMultipartUpload, GetStaleMultipartUploads, GetMultipartUploadsByBackend, ListMultipartUploadsByPrefix</p>'
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
        '<tr><td>last_error</td><td>TEXT</td><td>Most recent error message</td></tr>' +
        '<tr><td>claimed_at</td><td>TIMESTAMPTZ</td><td>NULL when unclaimed; set by ClaimPendingCleanups, cleared by RetryCleanupItem</td></tr>' +
        '<tr><td>claimed_by</td><td>TEXT</td><td>Stable instance identifier (hostname-XXXXXXXX) of the worker that holds the claim; observability only</td></tr></table>' +
        '<p class="ac-idx"><b>Indexes:</b> PK on id &bull; idx_cleanup_queue_claim (next_retry, created_at) WHERE attempts &lt; 10 (partial index, supports the ClaimPendingCleanups order-by-created_at filter without a sort)</p>' +
        '<p>Used by: enqueueCleanup() at all failure sites (PutObject, DeleteObject, multipart ops, <a href="../background-services/">rebalancer</a>, replicator), <a href="../background-services/">cleanupQueueService</a> background worker (runs every 1 min). The worker uses ClaimPendingCleanups (UPDATE...WHERE id IN (SELECT...FOR UPDATE SKIP LOCKED)) so concurrent ticks across instances return disjoint row sets; rows whose claim is older than <code>cleanup_queue.claim_grace_period</code> (default 5m) are reclaimable. On the tenth consecutive failure the row graduates to <code>cleanup_dlq</code> via <code>MoveCleanupToDLQ</code>; orphan_bytes is intentionally untouched there because the bytes are still on disk.</p>' +
        '<p class="ac-metric">Key queries: EnqueueCleanup, ClaimPendingCleanups (worker), GetPendingCleanups (admin/dashboard), CompleteCleanupItem (atomic delete + orphan_bytes decrement CTE), UpdateCleanupRetry, CountPendingCleanups, MoveCleanupToDLQ</p>' +
        '<p class="ac-metric">Metrics: s3o_cleanup_queue_enqueued_total, s3o_cleanup_queue_processed_total, s3o_cleanup_queue_depth, s3o_cleanup_queue_stale_claims_recovered_total{backend} &bull; Audit events: cleanup_queue.processed, cleanup_queue.claim_recovered, cleanup_queue.exhausted_to_dlq</p>'
    },
    cleanup_dlq: {
      title: 'cleanup_dlq',
      badge: 'cleanup', badgeText: 'dead-letter queue',
      body: '<p>Dead-letter table for cleanup_queue rows that exhausted their retry budget without ever succeeding at the physical backend delete. The row contents are preserved verbatim (key, backend, size, last_error) and the original_id correlates back to the queue row that was moved. Operators inspect this table to find unrecoverable orphans and retry them manually or write each entry off deliberately.</p>' +
        '<p><b>Important:</b> moving a row here does NOT decrement <code>orphan_bytes</code>. The backend object is still on disk; reclaim happens only when an operator confirms it is gone (e.g. via the reconciler) and runs a manual cleanup.</p>' +
        '<table class="ac-cols"><tr><th>Column</th><th>Type</th><th>Notes</th></tr>' +
        '<tr><td class="pk">id</td><td>BIGSERIAL</td><td>PRIMARY KEY (auto-increment)</td></tr>' +
        '<tr><td>original_id</td><td>BIGINT</td><td>cleanup_queue.id at the time of the move</td></tr>' +
        '<tr><td class="fk">backend_name</td><td>TEXT</td><td>FK &rarr; backend_quotas</td></tr>' +
        '<tr><td>object_key</td><td>TEXT</td><td>S3 key still on the backend</td></tr>' +
        '<tr><td>reason</td><td>TEXT</td><td>Original enqueue reason</td></tr>' +
        '<tr><td>size_bytes</td><td>BIGINT</td><td>Bytes still occupying backend quota</td></tr>' +
        '<tr><td>attempts</td><td>INT</td><td>Final attempt count (>= 10)</td></tr>' +
        '<tr><td>first_enqueued_at</td><td>TIMESTAMPTZ</td><td>Original cleanup_queue.created_at</td></tr>' +
        '<tr><td>moved_at</td><td>TIMESTAMPTZ</td><td>When the row was graduated</td></tr>' +
        '<tr><td>last_error</td><td>TEXT</td><td>Final backend-delete failure</td></tr></table>' +
        '<p class="ac-idx"><b>Indexes:</b> PK on id &bull; idx_cleanup_dlq_backend (backend_name)</p>' +
        '<p>Written by: <a href="../background-services/">cleanupQueueService</a> on retry exhaustion via <code>core.MoveCleanupToDLQ</code> (single transaction: read queue row, insert here, delete queue row).</p>' +
        '<p class="ac-metric">Key queries: InsertCleanupDLQ, CountCleanupDLQ</p>' +
        '<p class="ac-metric">Metrics: cleanup_dlq_depth (gauge), cleanup_dlq_enqueued_total{backend} (counter)</p>'
    },
    pending_objects: {
      title: 'pending_objects',
      badge: 'cleanup', badgeText: 'in-flight intents',
      body: '<p>In-flight PUT intents for the write-path PUT-before-COMMIT pattern. A row is inserted by <code>InsertPendingIntent</code> immediately before the backend PUT and deleted by <code>RecordObjectAndPromoteIntent</code> on a successful metadata commit. If the orchestrator dies between the backend PUT and the commit, the row survives and the <code>PendingReaper</code> worker resolves it on the next tick by HEADing the backend.</p>' +
        '<table class="ac-cols"><tr><th>Column</th><th>Type</th><th>Notes</th></tr>' +
        '<tr><td class="pk">intent_id</td><td>TEXT</td><td>PRIMARY KEY (UUID v4 minted by the coordinator)</td></tr>' +
        '<tr><td>object_key</td><td>TEXT</td><td>S3 key the PUT targets</td></tr>' +
        '<tr><td class="fk">backend_name</td><td>TEXT</td><td>FK &rarr; backend_quotas; target of the in-flight PUT</td></tr>' +
        '<tr><td>size_bytes</td><td>BIGINT</td><td>Ciphertext size (or plaintext if encryption disabled)</td></tr>' +
        '<tr><td>encrypted</td><td>BOOLEAN</td><td>true when an EncryptionMeta is captured below</td></tr>' +
        '<tr><td>encryption_key</td><td>BYTEA</td><td>Wrapped DEK (baseNonce || wrappedDEK) when encrypted</td></tr>' +
        '<tr><td>key_id</td><td>TEXT</td><td>Master key identifier for unwrap</td></tr>' +
        '<tr><td>plaintext_size</td><td>BIGINT</td><td>Logical object size (pre-encryption)</td></tr>' +
        '<tr><td>content_hash</td><td>TEXT</td><td>SHA-256 of the plaintext (when integrity is enabled)</td></tr>' +
        '<tr><td>compression_algorithm</td><td>TEXT</td><td>Codec the stored bytes were encoded with. NULL means stored verbatim, so no separate flag can drift out of step with it (nullable)</td></tr>' +
        '<tr><td>compression_level</td><td>TEXT</td><td>Level the object was written at. Diagnostic only, since decoding does not need it, but a rewrite pass does (nullable)</td></tr>' +
        '<tr><td>compression_format_version</td><td>SMALLINT</td><td>On-disk layout version, so a later change is detectable rather than silently misread (nullable)</td></tr>' +
        '<tr><td>logical_size</td><td>BIGINT</td><td>Size of the object the client wrote. Distinct from plaintext_size: with both features on the stored bytes are ciphertext of compressed data, so plaintext_size is the pre-encryption (compressed) size and this is the original (nullable)</td></tr>' +
        '<tr><td>created_at</td><td>TIMESTAMPTZ</td><td>Intent creation time; reaper only considers rows older than <code>write_path.pending_pattern.min_age</code> (default 5m)</td></tr></table>' +
        '<p class="ac-idx"><b>Indexes:</b> PK on intent_id &bull; idx_pending_objects_created (created_at) for the reaper\'s age-cursored scan &bull; idx_pending_objects_backend (backend_name)</p>' +
        '<p>Used by: <code>writepath.Coordinator.InsertPendingIntent</code> (inserts on PUT entry) &bull; <code>RecordObjectAndPromoteIntent</code> (delete on successful commit) &bull; <code>RecoverFromRecordFailure</code> (delete on drain race / commit failure) &bull; <a href="../background-services/">PendingReaper</a> (HEAD-probes the backend and promotes / drops stale intents). The reaper claims rows individually with <code>SELECT ... FOR UPDATE SKIP LOCKED</code>; no advisory lock is required because two reapers ticking concurrently always pick disjoint sets.</p>' +
        '<p class="ac-metric">Metrics: s3o_pending_intents_enqueued_total, s3o_pending_intents_resolved_total{status=committed|promoted|dropped|ambiguous|already_resolved}, s3o_pending_intents_depth &bull; Audit events: pending_reaper.promoted, pending_reaper.dropped, pending_reaper.superseded</p>'
    },
    notification_outbox: {
      title: 'notification_outbox',
      badge: 'usage', badgeText: 'notifications',
      body: '<p>Durable outbox queue for webhook event delivery. Events are inserted synchronously when state changes occur and drained asynchronously by a background worker that POSTs CloudEvents JSON to configured endpoints. Supports exponential backoff retries.</p>' +
        '<table class="ac-cols"><tr><th>Column</th><th>Type</th><th>Notes</th></tr>' +
        '<tr><td class="pk">id</td><td>BIGSERIAL</td><td>PRIMARY KEY (auto-increment)</td></tr>' +
        '<tr><td>event_type</td><td>TEXT</td><td>CloudEvents type (e.g. s3:ObjectCreated:Put)</td></tr>' +
        '<tr><td>payload</td><td>JSONB</td><td>Full CloudEvents JSON envelope</td></tr>' +
        '<tr><td>endpoint_url</td><td>TEXT</td><td>Webhook destination URL</td></tr>' +
        '<tr><td>created_at</td><td>TIMESTAMPTZ</td><td>Enqueue time</td></tr>' +
        '<tr><td>next_retry</td><td>TIMESTAMPTZ</td><td>Earliest delivery attempt time</td></tr>' +
        '<tr><td>attempts</td><td>INT</td><td>Delivery attempt count (max 10)</td></tr>' +
        '<tr><td>last_error</td><td>TEXT</td><td>Most recent delivery error</td></tr></table>' +
        '<p class="ac-idx"><b>Indexes:</b> PK on id &bull; idx_notification_outbox_pending (next_retry) WHERE attempts &lt; 10 (partial index)</p>' +
        '<p>Used by: <a href="../../guides/event-notifications/">event notifications</a> &mdash; emit() inserts rows, drainOnce() processes and delivers them via HTTP POST with optional HMAC signing.</p>' +
        '<p class="ac-metric">Metrics: notification_sent_total, notification_failed_total, notification_dropped_total, notification_queue_depth</p>'
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
      var tableNames = ['backend_quotas', 'object_locations', 'object_tags', 'multipart_uploads', 'multipart_parts', 'backend_usage', 'cleanup_queue', 'cleanup_dlq', 'pending_objects', 'notification_outbox'];
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
| <span style="color:#2a9d73">**PK**</span> | Primary key column |
| <span style="color:#5ec9a0">**FK**</span> | Foreign key reference |
| `\|\|--o{` | One-to-many relationship |
| <span style="color:#f0883e">**BIGSERIAL**</span> | Auto-incrementing surrogate key |
| <span style="color:#4aaa8a">**text_pattern_ops**</span> | B-tree index optimized for LIKE prefix queries |

### Table Summary

| Table | Purpose | Primary Key |
|-------|---------|-------------|
| **backend_quotas** | Backend registry with storage quota tracking | `backend_name` |
| **object_locations** | Maps objects to backends (supports replication) | `(object_key, backend_name)` |
| **object_tags** | Key/value labels on an object, shared by every replica | `(object_key, tag_key)` |
| **multipart_uploads** | In-progress multipart upload state | `upload_id` |
| **multipart_parts** | Individual parts within a multipart upload | `(upload_id, part_number)` |
| **backend_usage** | Monthly API/bandwidth counters per backend | `(backend_name, period)` |
| **cleanup_queue** | Retry queue for failed orphan deletions | `id` (auto-increment) |
| **cleanup_dlq** | Dead-letter for cleanup_queue rows that exhausted retries | `id` (auto-increment) |
| **pending_objects** | In-flight PUT intents (PUT-before-COMMIT write-path crash recovery) | `intent_id` (UUID) |
| **notification_outbox** | Durable webhook event delivery queue | `id` (auto-increment) |

### Schema Migrations

| Migration | Description |
|-----------|-------------|
| `00001_init_schema` | All six tables, indexes, and foreign keys |
| `00002_multipart_metadata` | Add `metadata` JSONB column to `multipart_uploads` |
| `00003_add_encryption` | Add encryption columns to `object_locations` and `multipart_parts` |
| `00004_add_orphan_bytes` | Add `orphan_bytes` to `backend_quotas` and `size_bytes` to `cleanup_queue` |
| `00005_add_content_hash` | Add `content_hash` to `object_locations` for integrity verification |
| `00006_add_indexes_and_tablesample` | Performance indexes on `multipart_uploads(backend_name)` and `object_locations(object_key, created_at)` |
| `00007_notification_outbox` | Add `notification_outbox` table for durable webhook event delivery |
| `00008_pending_objects` | Add `pending_objects` table for the PUT-before-COMMIT write-path pattern |
| `00009_cleanup_dlq` | Add `cleanup_dlq` table so retry-exhausted cleanup rows surface for operator action |
| `00010_multipart_upload_encryption` | Add `encryption_key` and `key_id` columns to `multipart_uploads` so every part of an encrypted upload shares one wrapped DEK |
| `00011_cleanup_queue_claim` | Add `claimed_at` and `claimed_by` to `cleanup_queue`; replace the partial index with `idx_cleanup_queue_claim (next_retry, created_at) WHERE attempts < 10`; supports the `ClaimPendingCleanups` `FOR UPDATE SKIP LOCKED` worker pattern that prevents cross-instance double-processing |
| `00012_reconcile_cursor_collation_index` | Add `idx_object_locations_backend_key_collate_c (backend_name, object_key COLLATE "C")` so the reconcile sorted-merge cursor's byte-ordered scan is index-backed |
| `00013_delimiter_prefix_collation_index` | Add `idx_object_locations_key_collate_c (object_key COLLATE "C")` so `ListObjectsDelimited` folds keys into `CommonPrefixes` with a loose index scan instead of a per-step sort |
| `00014_object_managed_flag` | Add `managed` to `object_locations` (default true) plus `idx_object_locations_managed (backend_name) WHERE managed`, so reconcile can import objects that sit outside every virtual bucket prefix for quota accounting without the workers acting on them |
| `00015_object_last_scrubbed_at` | Add `last_scrubbed_at` to `object_locations` (nullable) plus a partial index for the scrub queue. Replaces random sampling, which on Postgres could not reach past the front of the table: `TABLESAMPLE` walks the heap in physical order and `LIMIT` halts the scan, so most of the fleet was never verified |
| `00016_scrub_queue_last_touched` | Re-index the scrub queue on `COALESCE(last_scrubbed_at, created_at)` so a freshly written copy sorts behind an old unverified one. Ordering on the verified timestamp alone put every new write at the head, and a write rate above the scrub rate meant older data was never reached |
| `00017_compression_columns` | Add `compression_algorithm`, `compression_level`, `compression_format_version` and `logical_size` to `object_locations` and `pending_objects`. Nullable throughout, and a NULL algorithm means the bytes are stored verbatim, which is what every pre-existing row is, so no backfill is needed |
| `00018_compression_probe` | Add `compression_probe_size` and `compression_probe_level` to `object_locations`. Record what the encoder produced for a copy it declined to store compressed, so a later pass reaches the same verdict without downloading and encoding it again. The measurement is stored rather than a declined flag, because `min_ratio` is applied at query time: loosening it returns those copies to the pass with no read at all, where a flag would have to be found and cleared |
| `00019_object_tags` | Add `object_tags` (`object_key`, `tag_key`, `tag_value`) with `idx_object_tags_lookup (tag_key, tag_value)`. Keyed by object key alone, because tags describe the object and per-replica rows would let copies of one key disagree. No foreign key: nothing is keyed on object key alone, so `ON DELETE CASCADE` cannot express it and core clears the rows instead |
| `00020_multipart_tagging` | Add `tagging` to `multipart_uploads`, holding a query-string-encoded tag set from `CreateMultipartUpload` until completion. One column rather than a child table, since these are only ever read whole for one upload and never filtered by tag. Nullable, and NULL means the upload carried no tags, which is what every pre-existing row is |
