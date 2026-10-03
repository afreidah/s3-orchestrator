---
description: "Interactive diagram of a PutObject request through backend selection, encryption, failover, and the metadata recording steps."
title: "Write Path"
linkTitle: "Write Path"
weight: 2
---

Detailed flow of a PutObject request through backend selection, encryption, failover, and metadata recording. **Hover over any component** for implementation details.

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
    '    PUT([PutObject<br>Request]):::entry --> PREFLIGHT{CanAcceptWrite<br>Pre-flight}:::filter',
    '    PREFLIGHT -->|no backends| R507[507 Insufficient<br>Storage]:::reject',
    '    PREFLIGHT -->|ok| PLAN{Compress<br>This Write?}:::decision',
    '',
    '    PLAN -->|no| FILTER[Filter Eligible<br>Backends]:::filter',
    '    PLAN -->|yes| BUFFER',
    '    FILTER --> DRAIN[Exclude<br>Draining]:::filter',
    '    DRAIN --> HEALTH[Exclude<br>Unhealthy]:::filter',
    '    HEALTH --> USAGE[Usage Limits<br>Check]:::filter',
    '    USAGE -->|none eligible| R507B[507 Insufficient<br>Storage]:::reject',
    '    USAGE -->|eligible > 0| BUFFER[Buffer Body<br>+ Hashes]:::process',
    '',
    '    BUFFER -->|compressing| COMPRESS[Encode Chunked zstd<br>Seek Table]:::process',
    '    BUFFER -->|not compressing| ENC',
    '    COMPRESS --> RATIO{Shrank Past<br>min_ratio?}:::decision',
    '    RATIO -->|yes, keep encoding| ENC',
    '    RATIO -->|no, discard encoding| ENC',
    '',
    '    ENC{Encryption<br>Enabled?}:::decision',
    '    ENC -->|yes| ENCRYPT[Encrypt Once<br>DEK + AES-GCM]:::process',
    '    ENC -->|no| REFILTER',
    '    ENCRYPT --> REFILTER',
    '    REFILTER{Compressed<br>Write?}:::decision',
    '    REFILTER -->|yes| POSTFILTER[Filter Eligible<br>on Encoded Size]:::filter',
    '    REFILTER -->|no, filtered earlier| FANOUT',
    '    POSTFILTER -->|none eligible| R507C[507 Insufficient<br>Storage]:::reject',
    '    POSTFILTER -->|eligible > 0| FANOUT',
    '',
    '    FANOUT{Parallel<br>Copies On?}:::decision',
    '    FANOUT -->|yes| PARALLEL[Place Copies<br>in Parallel]:::process',
    '    FANOUT -->|no| RANK',
    '    PARALLEL -->|first copy commits| METRICS',
    '    PARALLEL -->|no slot to track it| RANK',
    '',
    '    RANK{Routing<br>Strategy}:::decision',
    '    RANK -->|spread| LEAST[Least Utilized<br>First]:::process',
    '    RANK -->|pack| FIRST[Configured<br>Order]:::process',
    '    LEAST --> INTENT',
    '    FIRST --> INTENT',
    '    INTENT[Claim Write Target<br>intent insert]:::storage',
    '    INTENT -->|declined, next candidate| INTENT',
    '    INTENT -->|none accept / DB down| FATAL[507 or 503<br>no failover]:::reject',
    '    INTENT -->|claimed| UPLOAD',
    '',
    '    UPLOAD[Upload to<br>Backend]:::process --> CB{Circuit<br>Breaker}:::decision',
    '    CB -->|open| FAIL',
    '    CB -->|closed| S3[S3 Backend<br>PutObject]:::storage',
    '    S3 -->|error, intent left for reaper| FAIL{Backends<br>Remain?}:::decision',
    '    S3 -->|success| DRAINRACE{IsDraining<br>re-check?}:::decision',
    '    DRAINRACE -->|drain seen| DRAINABORT[Drain Race Abort<br>delete bytes + failover]:::cleanup',
    '    DRAINRACE -->|not draining| RECORD',
    '    DRAINABORT --> FAIL',
    '',
    '    FAIL -->|yes| RETRY[Remove Backend<br>from Eligible]:::process',
    '    RETRY --> RANK',
    '    FAIL -->|no| RETERR[Return Last<br>Error]:::reject',
    '',
    '    RECORD[Record Object<br>atomic commit]:::storage',
    '    RECORD -->|commit fails| FATAL',
    '    RECORD --> DISPLACED{Displaced<br>Copies?}:::decision',
    '    DISPLACED -->|yes| CLEANUP[Delete Old Copies<br>or Enqueue Cleanup]:::cleanup',
    '    DISPLACED -->|no| METRICS',
    '    CLEANUP --> METRICS',
    '',
    '    METRICS[Record Usage<br>& Metrics]:::process --> CACHE[Invalidate<br>Caches]:::process',
    '    CACHE --> OK[Return ETag<br>200 OK]:::success',
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

  mermaid.render('write-mermaid-svg', diagramSrc).then(function(result) {
    document.getElementById('ac-diagram').innerHTML = result.svg;
    wireUpInteractivity();
  });

  var nodeInfo = {
    PUT: {
      title: 'PutObject Request',
      badge: 'entry', badgeText: 'entry point',
      body: '<p>Incoming PUT request after passing through admission control, rate limiting, and SigV4 authentication (header or presigned URL).</p><p>At this point <code>Content-Length</code> and <code>MaxObjectSize</code> have already been validated by the HTTP handler. User metadata (<code>x-amz-meta-*</code>) has been extracted and validated (max 2KB total).</p><p>Any <code>x-amz-tagging</code> header is parsed and validated right after the capacity pre-flight, still before the body is read. The header is query-string encoded (<code>k1=v1&amp;k2=v2</code>), and an unusable set is refused up front so a rejected write spends no ingress and leaves no orphan to collect. See <a href="../tagging/">tagging</a>.</p>'
    },
    PREFLIGHT: {
      title: 'CanAcceptWrite Pre-flight',
      badge: 'filter', badgeText: 'early rejection',
      body: '<p><code>CanAcceptWrite(contentLength)</code> runs <code>EligibleForWrite</code> (drain, health, then usage limits) on the client\'s <code>Content-Length</code> to check whether <b>any</b> backend can accept this upload.</p><p>Called <b>before</b> reading the request body. With <code>Expect: 100-Continue</code>, Go\'s net/http delays the 100 Continue response until the first <code>Body.Read()</code>, so the client never transmits bytes for a doomed upload.</p><p class="ac-metric">Metric: s3o_early_rejections_total</p>'
    },
    R507: {
      title: '507 Insufficient Storage',
      badge: 'reject', badgeText: 'rejection',
      body: '<p>No backend can accept this upload. Every backend is either draining, behind an open circuit breaker, or past its usage limits.</p><p>Returned before body transmission, saving bandwidth for both client and server.</p>'
    },
    PLAN: {
      title: 'Compress This Write?',
      badge: 'decision', badgeText: 'branch',
      body: '<p><code>compressOnWrite(size)</code>: compression is configured, <code>compression.enabled: true</code>, and the object is at least <code>min_size</code>. A seek table and per-frame headers cost more than a small object saves, so the floor avoids paying for no return.</p><p>Decided up front because it moves the eligibility filter. An uncompressed write is filtered before its body is buffered, so a full cluster rejects without spending a tempfile. A compressed write is filtered after encoding, on the size that will actually land, because rejecting on the logical size would turn away a write that fits.</p><p>Objects already stored compressed stay readable whether or not this is on, so the codec is built either way.</p>'
    },
    FILTER: {
      title: 'Filter Eligible Backends',
      badge: 'filter', badgeText: 'three-stage filter',
      body: '<p><code>EligibleForWrite</code> starts from the configured backend order and narrows it in three steps: <code>ExcludeDraining</code>, then <code>ExcludeUnhealthy</code>, then the usage-limit and <code>max_object_size</code> filter.</p><p>An uncompressed write runs this before its body is buffered, against the size the object will occupy once encrypted, which is known from the plaintext size alone. A compressed write runs the same chain after encoding instead; see <i>Filter Eligible on Encoded Size</i>.</p>'
    },
    USAGE: {
      title: 'Usage Limits Check',
      badge: 'filter', badgeText: 'usage filter',
      body: '<p>The usage policy\'s <code>FilterEligible</code>, which asks <code>UsageTracker.WithinLimits</code> about each backend:</p><p>1. <b>Request pools</b>: for every pool containing <code>PutObject</code>, baseline + current + 1 &le; that pool&#39;s limit. Providers meter operation classes separately, so a backend out of upload budget is filtered out here while its read allowance stays untouched<br>2. <b>Egress bytes</b>: baseline + current + 0 &le; limit<br>3. <b>Ingress bytes</b>: baseline + current + the stored size &le; limit</p><p>The size admitted is what will occupy the backend, not what the client announced. Encryption grows an object by a header plus a tag per chunk, which is a fixed function of the size and so is known before a byte moves. Compression is not: an encoder only reports its output size once it has run, which is why a compressed write is admitted after encoding rather than before.</p><p>Also skips backends where the object size exceeds <code>max_object_size</code> (0 = unlimited). Prevents repeated 413 errors from providers with per-object size restrictions.</p><p>Effective usage = DB baseline (cached) + in-memory deltas (from counter backend). Byte quota is not part of this check: it is decided at admission, by the intent insert.</p>'
    },
    DRAIN: {
      title: 'Exclude Draining',
      badge: 'filter', badgeText: 'drain filter',
      body: '<p>Removes backends that have a drain record: draining, drained, or failed.</p><p>Reads this instance\'s cached copy of the <code>backend_drains</code> records. It is loaded at startup, refreshed when this instance starts, cancels, or removes a drain, and reloaded on every usage-flush tick (default 30s), which runs on every instance; the instance running the drain worker also refreshes it on each pass. The cache only shapes the candidate list; the intent insert decides, reading the records through the <code>backend_capacity</code> view, so a write ranked against a stale cache is refused rather than admitted.</p>'
    },
    HEALTH: {
      title: 'Exclude Unhealthy',
      badge: 'filter', badgeText: 'health filter',
      body: '<p>Removes backends whose circuit breaker is <b>open</b>. Backends without a circuit breaker always pass; unknown backends are skipped.</p><p>Client requests are never used to test an open backend. Recovery runs out of band: the breaker watchdog has the backend\'s recovery prober run a <code>HeadBucket</code> health check once the open timeout has passed, then on a doubling backoff, and the breaker closes when a check passes. See the <a href="../circuit-breaker/">circuit breaker</a>.</p>'
    },
    R507B: {
      title: '507 Insufficient Storage',
      badge: 'reject', badgeText: 'rejection',
      body: '<p>After full filtering, no backends remain eligible. Returns <code>ErrInsufficientStorage</code>.</p><p class="ac-metric">Metric: s3o_usage_limit_rejections_total{operation="PutObject",limit_type="write"}</p>'
    },
    BUFFER: {
      title: 'Buffer Body + Hashes',
      badge: 'process', badgeText: 'buffering',
      body: '<p><code>materialize.New</code> buffers the request body into a seekable form: memory below 32 MiB, a self-unlinking tempfile above it, so heap does not scale with object size.</p><p>Necessary because <code>io.Reader</code> is single-use &mdash; if the upload fails and we need to retry on another backend, we need to replay the body. <code>Reader()</code> serves a fresh reader positioned at offset 0 on every call, and those readers are independent of one another, so a write placing several copies at once has one per upload.</p><p>Hashers ride along the same pass, so the body is never re-read: an MD5 of the plaintext for every write, which becomes the ETag the client is given, and, when <code>integrity.enabled: true</code>, a SHA-256 stored as <code>object_locations.content_hash</code> for read-time verification and the scrubber. Both are taken on the plaintext, so they describe the object the client wrote whatever form it is stored in.</p>'
    },
    RATIO: {
      title: 'Shrank Past min_ratio?',
      badge: 'decision', badgeText: 'branch',
      body: '<p>Compares the finished encoding against the original. An object that did not shrink to <code>min_ratio</code> of its original size is stored as the client sent it and the encoded copy is dropped, so the row carries no algorithm and no later read of it pays a decode.</p><p>This is what <code>min_size</code> cannot catch: media, archives and already-compressed content fail on entropy rather than size. Random data compresses to a ratio of exactly 1.000.</p><p>The decision is made on the finished encoding rather than a sample, because entropy is not uniform across an object and a sample is wrong in the direction that costs bytes for the life of the object. Encoding an object that turns out to be incompressible is the encoder\'s cheapest case: it detects unshrinkable blocks and stores them raw.</p>'
    },
    COMPRESS: {
      title: 'Encode Chunked zstd',
      badge: 'process', badgeText: 'compression',
      body: '<p>Encodes the buffered body into a second materialized body as one independently decodable zstd frame per <code>chunk_size</code> of input, with a seek table in a trailing skippable frame.</p><p>Runs once, ahead of the failover loop, and so does encryption: every attempt replays the finished payload and nothing is rebuilt per attempt. Ordering is compress then encrypt, because ciphertext does not compress.</p><p>The plaintext body is released once the encoded body exists; only the payload the uploads send is held from then on.</p><p>Records <code>compression_algorithm</code>, <code>compression_level</code>, <code>compression_format_version</code> and <code>logical_size</code> on the object row. <code>logical_size</code> is the only place the client-visible size survives, since <code>size_bytes</code> counts what landed on the backend.</p><p><a href="../compression/">Compression flow diagram &rarr;</a></p>'
    },
    ENC: {
      title: 'Encryption Enabled?',
      badge: 'decision', badgeText: 'branch',
      body: '<p>Checks whether an encryptor is configured (<code>encryption.enabled: true</code>).</p><p>If disabled, the buffered (or encoded) body is uploaded as it is. If enabled, it passes through envelope encryption first.</p>'
    },
    ENCRYPT: {
      title: 'Encrypt Once',
      badge: 'process', badgeText: 'encryption',
      body: '<p>Envelope encryption, run once ahead of the failover loop:</p><p>1. Generate a random 32-byte DEK (Data Encryption Key)<br>2. Wrap the DEK with the master key via the key provider (Vault Transit, KMS, or a local key)<br>3. Encrypt with AES-256-GCM in chunks (default 64 KiB) into a materialized body of its own, and release the plaintext</p><p>Every upload of this object replays that one ciphertext. Encrypting per attempt would draw a fresh base nonce each time, and copies of a key that differ byte for byte are something nothing downstream can detect, because each row is self-describing and reads and scrubs fine on its own.</p><p>The row records the stored form: <code>encrypted</code>, <code>encryption_key</code> (the packed <code>baseNonce || wrappedDEK</code>), <code>key_id</code>, and <code>plaintext_size</code>.</p><p class="ac-metric">Metric: s3o_encryption_operations_total{op="encrypt"}</p>'
    },
    REFILTER: {
      title: 'Compressed Write?',
      badge: 'decision', badgeText: 'branch',
      body: '<p>An uncompressed write was filtered before it was buffered and goes straight on. A compressed write has not been filtered yet, because the size that will land was unknown until the encoder ran.</p>'
    },
    POSTFILTER: {
      title: 'Filter Eligible on Encoded Size',
      badge: 'filter', badgeText: 'three-stage filter',
      body: '<p>The same <code>EligibleForWrite</code> chain as <i>Filter Eligible Backends</i> (drain, health, usage limits and <code>max_object_size</code>), run on the size of the payload that will actually be uploaded: the encoding, or its ciphertext when encryption is on.</p>'
    },
    R507C: {
      title: '507 Insufficient Storage',
      badge: 'reject', badgeText: 'rejection',
      body: '<p>No backend can take the encoded payload. Returns <code>ErrInsufficientStorage</code>.</p><p class="ac-metric">Metric: s3o_usage_limit_rejections_total{operation="PutObject",limit_type="write"}</p>'
    },
    FANOUT: {
      title: 'Parallel Copies On?',
      badge: 'decision', badgeText: 'branch',
      body: '<p>Whether <code>write_path.parallel_copies</code> is on with a copy count above one. Off by default, and a <code>replication.factor</code> of 1 leaves it inert.</p>'
    },
    PARALLEL: {
      title: 'Place Copies in Parallel',
      badge: 'process', badgeText: 'fan-out',
      body: '<p>Claims the top N eligible backends, each with an intent of its own (<code>ClaimWriteCopies</code>), and uploads to all of them at once from the one materialized payload. The client is answered as soon as the first copy commits, since waiting for the slowest backend would put it on the critical path of every write; the rest run on a context outliving the request and commit themselves as they land. What does not land is a shortfall the replicator fills, which is what it does for every copy when the gate is off.</p>' +
        '<p>The replicator makes a copy by reading the object back off a backend that holds it, so placing it here removes a full GET and that backend\'s egress - at the cost of sending those bytes at write time rather than spread across replicator cycles.</p>' +
        '<p>When <code>max_in_flight</code> copies are already uploading behind earlier responses, the fan-out is not attempted and the write falls through to the single-copy path below.</p>'
    },
    RANK: {
      title: 'Routing Strategy',
      badge: 'decision', badgeText: 'routing strategy',
      body: '<p>Orders the eligible backends for the claim. Ranking reads an in-memory snapshot of what each backend holds, reloaded on the usage service tick, so a stale ranking costs only an uneven spread that the next reload corrects.</p><p>Nothing here decides whether a backend has room; the intent insert does.</p>'
    },
    LEAST: {
      title: 'Least Utilized First',
      badge: 'process', badgeText: 'ranking',
      body: '<p><b>spread</b>: ranks the eligible backends emptiest first by utilization, from the in-memory snapshot (<code>QuotaTracker.RankByUtilization</code>).</p>'
    },
    FIRST: {
      title: 'Configured Order',
      badge: 'process', badgeText: 'ranking',
      body: '<p><b>pack</b>: keeps the eligible backends in configured order, so writes fill one backend before moving on. Useful for setups that exhaust cheap or local storage before spilling to cloud backends.</p>'
    },
    INTENT: {
      title: 'Claim Write Target',
      badge: 'storage', badgeText: 'conditional insert',
      body: '<p><code>ClaimWriteTarget</code> tries the ranked candidates in order, and for each runs the insert that writes the <code>pending_objects</code> row (<code>InsertPendingIfFits</code>, SQL <code>InsertPendingObjectIfFits</code>). It is one statement that inserts only if the backend has no drain record and still has room for the payload, both read through the <code>backend_capacity</code> view. Because it reads rows rather than memory, every instance is judged against the same totals.</p><p>A candidate that declines is skipped and the next one tried. When none accept, the write fails with 507; a database error fails it with 503. Neither is retried on another backend.</p><p>The row is written <b>before</b> the backend PUT. It holds the payload\'s bytes against the backend while the upload runs, and it records what recovery needs: key, backend, the path the bytes go to (the object key, <code>!</code>, and the intent\'s id), size, the stored form (encryption and compression fields), the object\'s identity (ETag, content type, user metadata), and its role.</p><p class="ac-metric">Metrics: s3o_pending_intents_enqueued_total, s3o_quota_claims_declined_total{backend}</p>'
    },
    FATAL: {
      title: '507 or 503, No Failover',
      badge: 'reject', badgeText: 'failure',
      body: '<p>Ends the request without trying another backend. Reached when no candidate accepts the intent insert (507 Insufficient Storage), or when the database fails during the claim or the commit (503 Service Unavailable).</p><p>A commit that fails leaves the intent and the uploaded bytes in place for the pending reaper to resolve.</p>'
    },
    UPLOAD: {
      title: 'Upload to Backend',
      badge: 'process', badgeText: 'upload',
      body: '<p>Calls the backend\'s <code>PutObject</code> with the prepared payload, under the per-backend timeout (<code>backend_timeout</code>). Each attempt takes a fresh reader over the same materialized payload, so a retry sends exactly the bytes the last attempt sent.</p><p>The size sent is the stored size: the ciphertext when encryption is on, the encoding when compression kept it.</p>'
    },
    CB: {
      title: 'Circuit Breaker',
      badge: 'decision', badgeText: 'circuit breaker',
      body: '<p><code>CircuitBreakerBackend.PutObject()</code> runs the real S3 call through <code>cb.Call</code>:</p><p><b>PreCheck</b>: if the circuit is open, return <code>ErrBackendUnavailable</code> immediately without I/O.<br><b>On failure</b>: only failures that say something about the backend\'s health count (network errors, 5xx, 429, 401/403); at the threshold the circuit opens.<br><b>Recovery</b>: the breaker closes only when the recovery prober\'s out-of-band health check passes, never on a client request.</p><p><a href="../circuit-breaker/">Circuit breaker state machine diagram &rarr;</a></p>'
    },
    S3: {
      title: 'S3 Backend PutObject',
      badge: 'storage', badgeText: 'S3 API call',
      body: '<p>AWS SDK v2 <code>s3.PutObject()</code> call to the backend endpoint. Builds <code>PutObjectInput</code> with bucket, key, body, content-length, content-type, and user metadata. The key sent is the intent\'s storage key, not the object key.</p><p>Supports <code>unsignedPayload</code> mode for backends that accept unsigned streaming uploads (avoids buffering for SigV4 signing). Returns ETag on success.</p><p class="ac-metric">Metrics: s3o_backend_requests_total, s3o_backend_duration_seconds</p>'
    },
    FAIL: {
      title: 'Upload Failed?',
      badge: 'decision', badgeText: 'failover',
      body: '<p>On a backend error (network timeout, S3 error, circuit breaker rejection) or a drain-race abort, the failed backend is recorded and removed from the eligible list, and the API call still counts against the backend\'s monthly limits.</p><p>The pending intent is left in place on a PUT error: the response may have been lost after the bytes landed, so the pending reaper HEADs the backend later and either promotes or drops the intent.</p><p>If backends remain, the loop claims a new target from the reduced list. When the write finally succeeds after failovers, each failed backend is counted against the one that took the write.</p><p class="ac-metric">Metric: s3o_write_failover_total{operation, failed_backend, success_backend}</p>'
    },
    RETRY: {
      title: 'Remove Backend from Eligible',
      badge: 'process', badgeText: 'failover',
      body: '<p>Removes the failed backend from the eligible list and logs a warning with the error, failed backend name, and count of remaining backends.</p><p>The next attempt ranks and claims again from the reduced list, and replays the same prepared payload, including the same ciphertext; nothing is re-encoded or re-encrypted.</p>'
    },
    RETERR: {
      title: 'Return Last Error',
      badge: 'reject', badgeText: 'failure',
      body: '<p>All eligible backends have been tried and failed. Returns the last error encountered, which the HTTP handler answers as a 502 Bad Gateway.</p><p>The span is marked with error status and the error is recorded for tracing.</p>'
    },
    RECORD: {
      title: 'RecordObjectAndPromoteIntent (atomic commit)',
      badge: 'storage', badgeText: 'DB transaction',
      body: '<p>Atomic database transaction (<code>RecordObjectAndPromoteIntent</code>) that turns the pending intent into a committed object_locations row:</p><p>1. <code>AcquireKeyLock</code> &mdash; key-scoped lock for concurrent write safety<br>2. <code>GetExistingCopiesForUpdate</code> &mdash; SELECT FOR UPDATE on current copies<br>3. <code>DeleteObjectCopies</code> &mdash; remove the existing copies, when there are any<br>4. Replace the tag set: delete the key\'s tags, then insert the set this request carried<br>5. <code>InsertObjectLocation</code> &mdash; one row per copy, with its stored form and identity<br>6. <code>AdjustQuotaStripe</code> &mdash; the freed and charged bytes together, on the stripe this key selects<br>7. <code>ClearPendingForKey</code> &mdash; delete every intent for the key except this write\'s copies still uploading</p><p>Step 6 is inside the transaction on purpose: the byte counter commits and rolls back with the rows it summarizes, so it cannot drift from them. Step 7 in the same transaction is what moves the write\'s bytes from what the backend has in flight to what it stores, without either total ever missing them.</p><p>Step 4 is why an untagged overwrite leaves the object untagged: tags follow the object, not the key, and the set is replaced inside the same transaction and under the same lock as the object itself, so there is no window where the new object carries the old object\'s tags.</p><p>Returns the <b>displaced copies</b> that need cleanup, each with the path its bytes occupy, plus the paths of other writes\' intents step 7 cleared. A copy on a backend this write also landed on is displaced like any other: the new bytes went to a path of their own, so the old copy is still sitting at its old path.</p><p>If this transaction fails, nothing is deleted: the intent and the uploaded bytes stay for the pending reaper, which HEADs the backend and promotes or drops the intent. The request fails without trying another backend, with 503 when the database is down.</p><p>A write placing several copies commits the first to land and carries the rest as <code>Placing</code>. Their intents survive step 7&#39;s by-key clear, which is what each late copy later reads as proof that nothing newer has taken the key. Their backends are not held back from step 3: each copy still uploading writes to its own intent&#39;s path, so deleting the previous copy on that backend cannot touch it.</p>'
    },
    DRAINRACE: {
      title: 'IsDraining Re-Check (drain race)',
      badge: 'decision', badgeText: 'drain race guard',
      body: '<p>Post-PUT guard. A drain can start <em>while</em> the backend PUT is in flight, after this write claimed its target, and committing would leave the drain an object to move straight back off.</p><p>This re-check fires after the PUT succeeds but before the metadata commit. If <code>IsDraining(backend)</code> is now true, the attempt is aborted and fails over to the next eligible backend.</p><p>It reads this instance\'s cached drain records, so a drain started on another instance is caught only once this one has seen it. A write that slips past is still safe: the drain cannot finish while the write\'s intent exists, and once the object commits the drain moves it.</p><p class="ac-metric">Metric: s3o_drain_race_aborted_total</p>'
    },
    DRAINABORT: {
      title: 'Drain Race Abort + Failover',
      badge: 'cleanup', badgeText: 'cleanup + failover',
      body: '<p>The bytes landed on a backend that started draining mid-write. <code>RecoverFromRecordFailure</code> issues a best-effort DELETE for them, enqueueing a cleanup row if the DELETE fails with anything other than 404. The pending intent is left for the reaper, which finds no object and drops it. The attempt then fails over to the next eligible backend.</p>'
    },
    DISPLACED: {
      title: 'Displaced Copies?',
      badge: 'decision', badgeText: 'overwrite check',
      body: '<p>When overwriting an existing object, every old copy needs to be cleaned up, including one on a backend this write also landed on. An overwrite never replaces bytes in place: the new copy has a path of its own, so a same-backend overwrite costs one extra DELETE.</p><p><code>RecordObject</code> returns the list of displaced copies with their backend names, paths and sizes. If the object didn\'t previously exist, this list is empty.</p>'
    },
    CLEANUP: {
      title: 'Delete Old Copies or Enqueue Cleanup',
      badge: 'cleanup', badgeText: 'cleanup',
      body: '<p>For each displaced copy:</p><p>1. Attempt an immediate <code>DeleteObject</code> at the copy\'s storage key<br>2. If delete fails: <code>enqueueCleanup()</code> &mdash; insert into <code>cleanup_queue</code> table with exponential backoff (1m to 24h, max 10 attempts)<br>3. <code>IncrementOrphanBytes()</code> on the backend\'s quota to prevent over-allocation while orphans exist</p><p>Audit event: <code>storage.overwrite_displaced</code> with count of displaced copies.</p><p class="ac-metric">Metric: s3o_cleanup_queue_enqueued_total{reason="overwrite_displaced"}</p>'
    },
    CACHE: {
      title: 'Invalidate Caches',
      badge: 'process', badgeText: 'cache',
      body: '<p>Removes the cached backend location for this key, and the cached object body when the object cache is on.</p><p>Ensures subsequent reads re-query the database to get the updated location, rather than reading from a stale cache entry pointing to the old backend.</p>'
    },
    METRICS: {
      title: 'Record Usage & Metrics',
      badge: 'process', badgeText: 'telemetry',
      body: '<p>Charges the successful PUT to the backend\'s monthly usage counters in the counter backend (local atomics or Redis): one request and the ingress. The charge carries the operation, so it lands on the backend&#39;s request total and on every budget pool that contains <code>PutObject</code>.</p><p>The ingress charged is the size the attempt actually sent, carried back from the upload rather than recomputed: the encoded bytes for a compressed object, the envelope for an encrypted one, and the ciphertext of the encoding when both are on. It is the same figure the ledger row commits, so the storage and bandwidth counters describe the object identically.</p><p>Also records the operation duration and, if failover occurred, <code>s3o_write_failover_total</code> for each failed backend paired with the one that took the write. Then the audit event (<code>storage.PutObject</code> with key, backend name, stored size) and the event notification are emitted.</p>'
    },
    OK: {
      title: 'Return ETag / 200 OK',
      badge: 'success', badgeText: 'success',
      body: '<p>Returns the ETag recorded on the object row: the MD5 of the plaintext the client sent, computed while the body was buffered. The HTTP handler sets the <code>ETag</code> response header and responds with <code>200 OK</code>.</p><p>The backend\'s own ETag is discarded, because with compression or encryption on it describes the stored bytes rather than the object the client wrote, and a later HEAD answers from the row.</p>'
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

## Pending-Intent Pattern

The `ClaimWriteTarget` then `RecordObjectAndPromoteIntent` two-phase pattern exists so a failure between the backend PUT and the metadata commit cannot leak orphan bytes. The intent row captures everything the recovery path needs (key, backend, storage path, size, stored form, identity), and the `PendingReaper` worker resolves intents older than its `min_age` on every tick (default 1 minute), under an advisory lock: it HEADs the backend and decides *commit* (HEAD 200: promote the intent, unless a newer write has already taken the key) or *drop* (HEAD 404: delete the intent; there are no bytes to clean up). Any other HEAD error leaves the intent for the next tick.

The intent row is also what admission counts as bytes in flight on its backend, so writes on every instance are judged against the same totals while uploads are still running. That is why the pattern cannot be turned off.

The post-PUT `IsDraining` re-check guards a separate race: a drain can begin while the backend PUT is in flight. Without the re-check, the bytes would land on the draining backend and the drain worker would have to move them off again. With it, the orchestrator aborts the attempt and fails over to the next eligible backend, incrementing `s3o_drain_race_aborted_total`. Either way the drain cannot finish while the write's intent exists.

See [`internal/worker/pending.go`](https://github.com/afreidah/s3-orchestrator/blob/main/internal/worker/pending.go) for the reaper implementation and [`internal/proxy/writepath/coordinator.go`](https://github.com/afreidah/s3-orchestrator/blob/main/internal/proxy/writepath/coordinator.go) for the coordinator-side helpers.

## Legend

| Color | Meaning |
|-------|---------|
| <span style="color:#1a7a5a">**Forest green**</span> | Entry point |
| <span style="color:#c4a35a">**Amber**</span> | Eligibility filtering |
| <span style="color:#2a9d73">**Green border**</span> | Decision / branch |
| <span style="color:#5ec9a0">**Teal**</span> | Processing step |
| <span style="color:#4aaa8a">**Teal**</span> | Storage / DB / S3 |
| <span style="color:#34b882">**Green**</span> | Success |
| <span style="color:#d4a0a0">**Red**</span> | Rejection / failure |
| <span style="color:#8a9aa8">**Gray**</span> | Cleanup |
