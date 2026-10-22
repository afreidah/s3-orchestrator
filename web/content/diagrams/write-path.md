---
description: "Interactive diagram of a PutObject request through backend selection, encryption, failover, and the metadata recording steps."
title: "Write Path"
linkTitle: "Write Path"
weight: 2
---

Detailed flow of a PutObject request through backend selection, encryption, failover, and metadata recording. **Hover over any component** for implementation details.

{{< diagram >}}

<script>
window.s3oDiagram = {
  source: [
    'flowchart TD',
    '    PUT([PutObject<br>Request]):::entry --> PREFLIGHT{CanAcceptWrite<br>Pre-flight}:::filter',
    '    PREFLIGHT -->|no backends| R507[507 Insufficient<br>Storage]:::reject',
    '    PREFLIGHT -->|ok| PLAN{Compress<br>This Write?}:::decision',
    '',
    '    PLAN -->|no| FILTER[Filter Eligible<br>Backends]:::filter',
    '    PLAN -->|yes| BUFFER',
    '    FILTER --> DRAIN[Exclude Cordoned<br>or Draining]:::filter',
    '    DRAIN --> HEALTH[Exclude Refusing<br>PUT or GET]:::filter',
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
    '    PARALLEL -->|claim fails| FATAL',
    '    PARALLEL -->|every upload fails| RETERR',
    '',
    '    RANK{Routing<br>Strategy}:::decision',
    '    RANK -->|spread| LEAST[Least Utilized<br>First]:::process',
    '    RANK -->|pack| FIRST[Configured<br>Order]:::process',
    '    LEAST --> INTENT',
    '    FIRST --> INTENT',
    '    INTENT[Claim Write Target<br>intent insert]:::storage',
    '    INTENT -->|declined, next candidate| INTENT',
    '    INTENT -->|none accept / store error| FATAL[507, 503 or 502<br>no failover]:::reject',
    '    INTENT -->|claimed| UPLOAD',
    '',
    '    UPLOAD[Upload to<br>Backend]:::process --> CB{Circuit<br>Breaker}:::decision',
    '    CB -->|open| FAIL',
    '    CB -->|closed| S3[S3 Backend<br>PutObject]:::storage',
    '    S3 -->|error, intent left for reaper| FAIL{Backends<br>Remain?}:::decision',
    '    S3 -->|success| DRAINRACE{Write target<br>re-check?}:::decision',
    '    DRAINRACE -->|cordoned or draining| DRAINABORT[Write Target Refused<br>discard bytes + failover]:::cleanup',
    '    DRAINRACE -->|still accepts writes| RECORD',
    '    DRAINABORT --> FAIL',
    '',
    '    FAIL -->|yes| RETRY[Remove Backend<br>from Eligible]:::process',
    '    RETRY --> RANK',
    '    FAIL -->|no| RETERR[Return Last<br>Error]:::reject',
    '',
    '    RECORD[Record Object<br>atomic commit]:::storage',
    '    RECORD -->|commit fails, intent left for reaper| FATAL',
    '    RECORD --> DISPLACED{Displaced<br>Copies?}:::decision',
    '    DISPLACED -->|yes| CLEANUP[Delete Queued<br>Old Copies]:::cleanup',
    '    DISPLACED -->|no| METRICS',
    '    CLEANUP --> METRICS',
    '',
    '    METRICS[Record Usage<br>& Metrics]:::process --> CACHE[Invalidate<br>Caches]:::process',
    '    CACHE --> OK[Return ETag<br>200 OK]:::success',
  ].join('\n'),

  info: {
    PUT: {
      title: 'PutObject Request',
      badge: 'entry', badgeText: 'entry point',
      body: '<p>Incoming PUT request after passing through admission control, rate limiting, and SigV4 authentication (header or presigned URL).</p><p>At this point <code>Content-Length</code> and <code>MaxObjectSize</code> have already been validated by the HTTP handler. User metadata (<code>x-amz-meta-*</code>) has been extracted and validated (max 2KB total). A request carrying <code>If-None-Match: *</code> is checked against the key\'s existing copies next, before the capacity pre-flight, so a write that would be refused for clobbering a key transmits nothing.</p><p>Any <code>x-amz-tagging</code> header is parsed and validated right after the capacity pre-flight, still before the body is read. The header is query-string encoded (<code>k1=v1&amp;k2=v2</code>), and an unusable set is refused up front so a rejected write spends no ingress and leaves no orphan to collect. See <a href="../tagging/">tagging</a>.</p>'
    },
    PREFLIGHT: {
      title: 'CanAcceptWrite Pre-flight',
      badge: 'filter', badgeText: 'early rejection',
      body: '<p><code>CanAcceptWrite(contentLength)</code> runs <code>EligibleForWrite</code> (drain, health, then usage limits) on the client\'s <code>Content-Length</code> to check whether <b>any</b> backend can accept this upload.</p><p>Called <b>before</b> reading the request body. With <code>Expect: 100-Continue</code>, Go\'s net/http delays the 100 Continue response until the first <code>Body.Read()</code>, so the client never transmits bytes for a doomed upload.</p><p class="ac-metric">Metric: s3o_early_rejections_total</p>'
    },
    R507: {
      title: '507 Insufficient Storage',
      badge: 'reject', badgeText: 'rejection',
      body: '<p>No backend can accept this upload. Every backend is cordoned or carries a drain flag, is refusing PUT or GET behind an open circuit breaker, or is past its usage limits or <code>max_object_size</code>.</p><p>Returned before body transmission, saving bandwidth for both client and server.</p>'
    },
    PLAN: {
      title: 'Compress This Write?',
      badge: 'decision', badgeText: 'branch',
      body: '<p><code>compressOnWrite(size)</code>: compression is configured, <code>compression.enabled: true</code>, and the object is at least <code>min_size</code>. A seek table and per-frame headers cost more than a small object saves, so the floor avoids paying for no return.</p><p>Decided up front because it moves the eligibility filter. An uncompressed write is filtered before its body is buffered, so a full cluster rejects without spending a tempfile. A compressed write is filtered after encoding, on the size that will actually land, because rejecting on the logical size would turn away a write that fits.</p><p>Objects already stored compressed stay readable whether or not this is on, so the codec is built either way.</p>'
    },
    FILTER: {
      title: 'Filter Eligible Backends',
      badge: 'filter', badgeText: 'three-stage filter',
      body: '<p><code>EligibleForWrite</code> starts from <code>WriteTargets</code> - the configured backend order narrowed by <code>Accepted</code>, then <code>Serving</code> - and drops the backends failing the usage-limit and <code>max_object_size</code> filter.</p><p>An uncompressed write runs this before its body is buffered, against the size the object will occupy once encrypted, which is known from the plaintext size alone. A compressed write runs the same chain after encoding instead; see <i>Filter Eligible on Encoded Size</i>.</p>'
    },
    USAGE: {
      title: 'Usage Limits Check',
      badge: 'filter', badgeText: 'usage filter',
      body: '<p><code>EligibleForWrite</code>\'s usage filter, which asks <code>UsageTracker.WithinLimits</code> about each backend:</p><p>1. <b>Request pools</b>: for every pool containing <code>PutObject</code>, baseline + current + 1 &le; that pool&#39;s limit. Providers meter operation classes separately, so a backend out of upload budget is filtered out here while its read allowance stays untouched<br>2. <b>Egress bytes</b>: baseline + current + 0 &le; limit<br>3. <b>Ingress bytes</b>: baseline + current + the stored size &le; limit</p><p>The size admitted is what will occupy the backend, not what the client announced. Encryption grows an object by a header plus a tag per chunk, which is a fixed function of the size and so is known before a byte moves. Compression is not: an encoder only reports its output size once it has run, which is why a compressed write is admitted after encoding rather than before.</p><p>Also skips backends where the object size exceeds <code>max_object_size</code> (0 = unlimited). Prevents repeated 413 errors from providers with per-object size restrictions.</p><p>Effective usage = DB baseline (cached) + in-memory deltas (from counter backend). Byte quota is not part of this check: it is decided at admission, by the intent insert.</p>'
    },
    DRAIN: {
      title: 'Exclude Cordoned or Draining',
      badge: 'filter', badgeText: 'drain filter',
      body: '<p>Removes backends carrying a state flag writes do not accept: cordoned, or a drain that is draining, drained, or failed.</p><p>Reads this instance\'s cached copy of the <code>backend_states</code> flags. It is loaded at startup, refreshed when this instance cordons, uncordons, starts, cancels, or removes a drain, and reloaded on every usage-flush tick (default 30s, shortening to <code>usage_flush.fast_interval</code> near a limit), which runs on every instance; the instance running the drain worker also refreshes it on each pass. The cache only shapes the candidate list; the intent insert decides, reading the records through the <code>backend_capacity</code> view, so a write ranked against a stale cache is refused rather than admitted.</p>'
    },
    HEALTH: {
      title: 'Exclude Refusing PUT or GET',
      badge: 'filter', badgeText: 'health filter',
      body: '<p><code>WriteTargets</code> applies <code>RequiredForWrite</code>, removing backends not serving both <code>PutObject</code> and <code>GetObject</code>: the backend breaker, the PUT breaker or the GET breaker is <b>open</b>. A copy on a backend refusing downloads, such as one over a provider quota, could not be read back or replicated, so it gets no new objects. Drains and rebalance moves pick their destinations from the same list. Backends without circuit breakers always pass; unknown backends are skipped.</p><p>Client requests are never used to test an open breaker. Recovery runs out of band: the breaker watchdog has each open breaker\'s recovery prober run its check - <code>HeadBucket</code> for the backend breaker, a repeat of the call for a call breaker - once the open timeout has passed, then on a doubling backoff, and the breaker closes when a check passes. See the <a href="../circuit-breaker/">circuit breaker</a>.</p>'
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
      body: '<p>Claims the top N eligible backends, each with an intent of its own, in one transaction (<code>ClaimWriteCopies</code>, store <code>ClaimPendingCopies</code>): every capped candidate is locked first in name order, so two writes ranking the same backends differently cannot deadlock, then the ranked candidates run the conditional insert until every copy has one. A backend without room is skipped, and an error rolls back every claim. It then uploads to all of them at once from the one materialized payload. The client is answered as soon as the first copy commits, since waiting for the slowest backend would put it on the critical path of every write; the rest run on a context outliving the request and commit themselves as they land. What does not land is a shortfall the replicator fills, which is what it does for every copy when the gate is off.</p>' +
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
      body: '<p><code>ClaimWriteTarget</code> tries the ranked candidates in order, and for each runs the insert that writes the <code>pending_objects</code> row (<code>InsertPendingIfFits</code>, SQL <code>InsertPendingObjectIfFits</code>). It is one statement that inserts only if the backend carries no state flag writes refuse (cordoned, or any drain flag) and still has room for the payload, both read through the <code>backend_capacity</code> view. Because it reads rows rather than memory, every instance is judged against the same totals. On a backend with a byte ceiling the insert runs after a transaction-scoped lock on the backend, so two claims checked at once are judged against each other\'s bytes rather than each seeing the room before the other; an unlimited backend takes no lock.</p><p>A candidate that declines is skipped and the next one tried. When none accept, the write fails with 507; a database outage (its circuit open) fails it with 503, and any other store error with 502. None is retried on another backend.</p><p>The row is written <b>before</b> the backend PUT. It holds the payload\'s bytes against the backend while the upload runs, and it records what recovery needs: key, backend, the path the bytes go to (the object key, <code>!</code>, and the intent\'s id), size, the stored form (encryption and compression fields), the object\'s identity (ETag, content type, user metadata), and its role.</p><p class="ac-metric">Metrics: s3o_pending_intents_enqueued_total, s3o_quota_claims_declined_total{backend}</p>'
    },
    FATAL: {
      title: '507, 503 or 502, No Failover',
      badge: 'reject', badgeText: 'failure',
      body: '<p>Ends the request without trying another backend. Reached when no candidate accepts the intent insert (507 Insufficient Storage), or when the store fails during the claim or the commit: 503 Service Unavailable while the database circuit is open, 502 for any other store error. The parallel path ends here too when its claim fails.</p><p>A commit that fails leaves the intent and the uploaded bytes in place for the pending reaper to resolve.</p>'
    },
    UPLOAD: {
      title: 'Upload to Backend',
      badge: 'process', badgeText: 'upload',
      body: '<p>Calls the backend\'s <code>PutObject</code> with the prepared payload, under the per-backend timeout (<code>backend_timeout</code>). Each attempt takes a fresh reader over the same materialized payload, so a retry sends exactly the bytes the last attempt sent.</p><p>The size sent is the stored size: the ciphertext when encryption is on, the encoding when compression kept it.</p>'
    },
    CB: {
      title: 'Circuit Breaker',
      badge: 'decision', badgeText: 'circuit breaker',
      body: '<p><code>CircuitBreakerBackend.PutObject()</code> runs the real S3 call through the backend breaker and then the PUT breaker:</p><p><b>PreCheck</b>: if either is open, return <code>ErrBackendUnavailable</code> immediately without I/O.<br><b>On failure</b>: network errors and 401 count toward the backend breaker, which stops every call; 5xx, 429 and 403 count toward the PUT breaker alone. At the threshold the breaker opens.<br><b>Recovery</b>: a breaker closes only when its recovery prober\'s out-of-band check passes, never on a client request. The PUT breaker\'s check writes the reserved empty object <code>.s3o-probe</code>.</p><p><a href="../circuit-breaker/">Circuit breaker state machine diagram &rarr;</a></p>'
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
      body: '<p>Atomic database transaction (<code>RecordObjectAndPromoteIntent</code>) that turns the pending intent into a committed object_locations row:</p><p>1. <code>AcquireKeyLock</code> &mdash; key-scoped lock for concurrent write safety<br>2. <code>GetExistingCopiesForUpdate</code> &mdash; SELECT FOR UPDATE on current copies<br>3. <code>DeleteObjectCopies</code> &mdash; remove the existing copies, when there are any<br>4. Replace the tag set: delete the key\'s tags, then insert the set this request carried<br>5. <code>InsertObjectLocation</code> &mdash; one row per copy, with its stored form and identity<br>6. <code>AdjustQuotaStripe</code> &mdash; the freed and charged bytes together, on the stripe this key selects<br>7. <code>ClearPendingForKey</code> &mdash; delete every intent for the key except this write\'s copies still uploading<br>8. <code>InsertCleanup</code> &mdash; queue the bytes of the displaced copies and of the intents step 7 cleared in <code>cleanup_queue</code>, already claimed by this instance</p><p>Step 6 is inside the transaction on purpose: the byte counter commits and rolls back with the rows it summarizes, so it cannot drift from them. Step 7 in the same transaction is what moves the write\'s bytes from what the backend has in flight to what it stores, without either total ever missing them. Step 8 is why no displaced bytes go untracked: their cleanup rows commit with the overwrite, and count as the backend\'s orphan bytes until they are deleted.</p><p>Step 4 is why an untagged overwrite leaves the object untagged: tags follow the object, not the key, and the set is replaced inside the same transaction and under the same lock as the object itself, so there is no window where the new object carries the old object\'s tags.</p><p>Returns the cleanup rows it queued for the <b>displaced copies</b>, each with the path its bytes occupy, plus the paths of other writes\' intents step 7 cleared. A copy on a backend this write also landed on is displaced like any other: the new bytes went to a path of their own, so the old copy is still sitting at its old path.</p><p>If this transaction fails, nothing is deleted: the intent and the uploaded bytes stay for the pending reaper, which HEADs the backend and promotes or drops the intent. The request fails without trying another backend, with 503 when the database is down.</p><p>A write placing several copies commits the first to land and carries the rest as <code>Placing</code>. Their intents survive step 7&#39;s by-key clear, which is what each late copy later reads as proof that nothing newer has taken the key. Their backends are not held back from step 3: each copy still uploading writes to its own intent&#39;s path, so deleting the previous copy on that backend cannot touch it.</p>'
    },
    DRAINRACE: {
      title: 'Write Target Re-Check',
      badge: 'decision', badgeText: 'state race guard',
      body: '<p>Post-PUT guard. A backend can be cordoned or start draining <em>while</em> the backend PUT is in flight, after this write claimed its target. Committing would put a new copy on a backend the operator took out of service, or leave a drain an object to move straight back off.</p><p>This re-check fires after the PUT succeeds but before the metadata commit. If the backend\'s flags are no longer ones writes accept, the attempt is aborted and fails over to the next eligible backend.</p><p>It reads this instance\'s cached backend state flags, so a change made on another instance is caught only once this one has seen it. A write that slips past a drain is still safe: the drain cannot finish while the write\'s intent exists, and once the object commits the drain moves it.</p><p class="ac-metric">Metric: s3o_write_target_refused_total</p>'
    },
    DRAINABORT: {
      title: 'Write Target Refused + Failover',
      badge: 'cleanup', badgeText: 'cleanup + failover',
      body: '<p>The bytes landed on a backend that was cordoned or started draining mid-write. The attempt charges the PUT that landed, then <code>Coordinator.Discard</code> drops the intent and queues the bytes in <code>cleanup_queue</code> with reason <code>write_target_refused</code> in one transaction, and deletes them once it commits. If the discard itself fails, the intent stays for the pending reaper to resolve. The attempt then fails over to the next eligible backend.</p>'
    },
    DISPLACED: {
      title: 'Displaced Copies?',
      badge: 'decision', badgeText: 'overwrite check',
      body: '<p>When overwriting an existing object, every old copy needs to be cleaned up, including one on a backend this write also landed on. An overwrite never replaces bytes in place: the new copy has a path of its own, so a same-backend overwrite costs one extra DELETE.</p><p><code>RecordObject</code> returns the cleanup rows it queued for the displaced copies, with their backend names, paths and sizes. If the object didn\'t previously exist, this list is empty.</p>'
    },
    CLEANUP: {
      title: 'Delete Queued Old Copies',
      badge: 'cleanup', badgeText: 'cleanup',
      body: '<p>The commit already queued the displaced copies in <code>cleanup_queue</code>, claimed by this instance. After it commits, the copies are deleted together, one delete per backend, batched when a backend holds several paths, and each row is settled on its own result:</p><p>1. Delete at the copy\'s storage key<br>2. Success or 404: retire the row, which releases its bytes from the backend\'s orphan bytes<br>3. Failure: leave the row for the cleanup worker with exponential backoff (1m to 24h, max 10 attempts)</p><p>A client that disconnects mid-delete does not cancel the row outcomes, so an unfinished delete is retried rather than stuck. A process that dies before settling leaves the rows claimed, and the worker reclaims them after <code>cleanup_queue.claim_grace_period</code>.</p><p>Audit event: <code>storage.overwrite_displaced</code> with count of displaced copies.</p><p class="ac-metric">Metric: s3o_cleanup_queue_enqueued_total{reason="overwrite_displaced"}, s3o_cleanup_queue_processed_total{status}</p>'
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
  }
};
</script>

## Pending-Intent Pattern

The `ClaimWriteTarget` then `RecordObjectAndPromoteIntent` two-phase pattern exists so a failure between the backend PUT and the metadata commit cannot leak orphan bytes. The intent row captures everything the recovery path needs (key, backend, storage path, size, stored form, identity), and the `PendingReaper` worker runs every `reaper_tick` (default 1 minute), under an advisory lock, on intents older than `min_age` (default 5 minutes): it HEADs the backend and decides *commit* (HEAD 200: promote the intent, unless a newer write has already taken the key, in which case its bytes are queued for deletion as `superseded_intent`) or *drop* (HEAD 404: delete the intent; there are no bytes to clean up). An extra copy's intent is a companion and is never promoted: it is kept when a recorded copy sits at its path and otherwise discarded with its bytes. Any other HEAD error leaves the intent for the next tick, and an intent on a backend refusing `HeadObject` is skipped until it serves HEADs again.

The intent row is also what admission counts as bytes in flight on its backend, so writes on every instance are judged against the same totals while uploads are still running. That is why the pattern cannot be turned off.

The post-PUT write target re-check guards a separate race: a backend can be cordoned or start draining while the backend PUT is in flight. Without the re-check, the bytes would land on a backend the operator took out of service, and a drain worker would have to move them off again. With it, the orchestrator aborts the attempt and fails over to the next eligible backend, incrementing `s3o_write_target_refused_total`. Either way a drain cannot finish while the write's intent exists.

See [`internal/worker/pending.go`](https://github.com/afreidah/s3-orchestrator/blob/main/internal/worker/pending.go) for the reaper implementation and [`internal/proxy/writepath/coordinator.go`](https://github.com/afreidah/s3-orchestrator/blob/main/internal/proxy/writepath/coordinator.go) for the coordinator-side helpers.

## Legend

| Node | Meaning |
|------|---------|
| <span class="dg-swatch dg-entry"></span> | Entry point |
| <span class="dg-swatch dg-filter"></span> | Eligibility filtering |
| <span class="dg-swatch dg-decision"></span> | Decision / branch |
| <span class="dg-swatch dg-process"></span> | Processing step |
| <span class="dg-swatch dg-storage"></span> | Storage / DB / S3 |
| <span class="dg-swatch dg-success"></span> | Success |
| <span class="dg-swatch dg-reject"></span> | Rejection / failure |
| <span class="dg-swatch dg-background"></span> | Cleanup |
