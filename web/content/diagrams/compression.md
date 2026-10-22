---
description: "Interactive diagram of at-rest compression: how an object is encoded on write, reassembled on read, and why the format is chunked."
title: "Compression Flow"
linkTitle: "Compression Flow"
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
4. **Encrypt, if enabled**: compression runs first, in that order only, because ciphertext does not compress. A verbatim object is encrypted the same way.
5. **Admit against the final upload size**: unlike encryption, whose overhead is a fixed function of the size, an encoder only reports its output once it has run. A write that reached the encoder is therefore admitted after its body is prepared, against the bytes that will be uploaded (encoded, then enveloped when encrypted). Each upload attempt then claims capacity on its target backend.
6. **Upload and record the stored form**: `compression_algorithm`, `compression_level`, `compression_format_version` and `logical_size`. A NULL algorithm is the ledger's way of saying the bytes are verbatim, so no separate boolean can drift out of step with it.

#### Reading an object

A compressed copy is never served by a whole-object GET. The codec drives the read instead:

1. **Read the seek table** from the trailing skippable frame, which maps a logical offset to the frame holding it.
2. **Map the client's range** onto the frames covering it.
3. **Fetch those frames** with one ranged backend GET each, decrypting them first when the copy is an envelope. The seek table read pulls the last 8 KiB of the object in one fetch, and frames inside that tail need no further GET.
4. **Decode and slice** to the exact bytes the client asked for.

Each frame fetch is charged its own API call and egress, on the bytes that actually left the backend. Charging once per client request would under-report all but the first.

#### The two thresholds

`min_size` and `min_ratio` exist because an object can fail to benefit for two different reasons. `min_size` is about framing overhead on small objects; `min_ratio` is about entropy - already-compressed content, media and archives gain nothing at any size. Objects declined by either are stored exactly as the client sent them, and nothing on the read path distinguishes the two cases, because nothing needs to.

{{< diagram >}}

<script>
window.s3oDiagram = {
  source: [
    'flowchart TD',
    '    PUT([PutObject or<br>Multipart Complete]):::entry --> MINSIZE{size at least<br>min_size?}:::filter',
    '',
    '    MINSIZE -->|no: admitted on enveloped<br>size before buffering| VERBATIM[Store Verbatim]:::process',
    '    MINSIZE -->|yes| ENCODE[Encode: one zstd frame<br>per chunk_size]:::process',
    '    ENCODE --> SEEKTBL[Append Seek Table<br>skippable frame]:::process',
    '    SEEKTBL --> RATIO{encoded within min_ratio<br>of original?}:::filter',
    '',
    '    RATIO -->|no| DISCARD[Discard Encoding]:::reject',
    '    DISCARD --> VERBATIM',
    '    RATIO -->|yes| ENCRYPT{Encryption<br>Enabled?}:::decision',
    '    VERBATIM --> ENCRYPT',
    '',
    '    ENCRYPT -->|yes| ENVELOPE[Encrypt the<br>Prepared Body]:::process',
    '    ENCRYPT -->|no| ADMIT',
    '    ENVELOPE --> ADMIT[Admit on<br>Upload Size]:::filter',
    '    ADMIT --> UPLOAD[Upload to<br>Backend]:::storage',
    '',
    '    UPLOAD --> ROW[Record Stored Form<br>on the Ledger Row]:::success',
    '',
    '    GETR([GetObject]):::entry --> ISCOMP{Row carries an<br>algorithm?}:::decision',
    '    ISCOMP -->|no| PLAINREAD[Whole-object GET]:::storage',
    '    ISCOMP -->|yes| RDTABLE[Fetch Seek Table]:::storage',
    '',
    '    RDTABLE --> MAPFRAME[Map Range to<br>Covering Frames]:::process',
    '    MAPFRAME --> FETCHF[Ranged GET<br>per Needed Frame]:::storage',
    '    FETCHF --> DECR{Copy<br>Encrypted?}:::decision',
    '',
    '    DECR -->|yes| DECRYPTF[Decrypt Ciphertext<br>Chunks]:::process',
    '    DECR -->|no| DECODEF',
    '    DECRYPTF --> DECODEF[Decode Frames]:::process',
    '    DECODEF --> SLICEF[Slice to<br>Client Range]:::process',
    '    SLICEF --> SERVE[Stream to<br>Client]:::success',
    '    PLAINREAD --> SERVE',
  ].join('\n'),

  info: {
    PUT: {
      title: 'PutObject or Multipart Complete',
      badge: 'entry', badgeText: 'entry point',
      body: '<p>Both write paths encode, and both encode once. A single PUT encodes and encrypts once, ahead of the failover loop, so a retry rewinds and resends the same materialized ciphertext (or encoded bytes when encryption is off).</p><p>A multipart upload is encoded when its parts are assembled, not part by part as they arrive. Its chunk layout therefore owes nothing to the part sizes the client chose, which matters because those are arbitrary: a client picking 8 MiB parts and one picking 500 MiB parts produce identically seekable objects.</p><p><a href="../write-path/">Write path diagram &rarr;</a></p>'
    },
    MINSIZE: {
      title: 'Size Floor (min_size)',
      badge: 'filter', badgeText: 'threshold',
      body: '<p>Objects below <code>compression.min_size</code> (default 4096) are stored verbatim.</p><p>A seek table and per-frame headers are a fixed cost per object. Below some size that cost exceeds anything the encoding could save, so the floor avoids paying it for no return.</p><p>This is the only skip that is free: it is answerable from the size alone, so an object below it costs no encode at all.</p><p class="ac-metric">Metric: s3o_compression_skipped_total{reason="min_size"}</p>'
    },
    ENCODE: {
      title: 'Encode: zstd Frames',
      badge: 'process', badgeText: 'encoding',
      body: '<p>The buffered body is encoded into a second buffer as one independently decodable zstd frame per <code>chunk_size</code> of input (default 1 MiB, range 16 KiB to 64 MiB).</p><p>Each stage releases the buffer it consumed: the plaintext is freed as soon as the encoding is kept, and the encoding is freed once the ciphertext exists. Only the final payload is held through the failover loop. Bodies above 32 MiB spill to a self-unlinking tempfile rather than being held on the heap.</p><p>The level is a name rather than a number - <code>fastest</code>, <code>default</code>, <code>better</code>, <code>best</code> - because zstd collapses its numeric 1-19 range into four buckets, and a numeric setting would let an operator express a distinction the encoder discards.</p><p><a href="../../docs/compression/">Compression reference &rarr;</a></p>'
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
      body: '<p>On a single PUT the encoded buffer is released and the plan goes back to the plaintext it was made from. On multipart completion the plaintext is not kept, so a rejected encoding is decoded back out of its buffer and that decoded stream is uploaded.</p><p>Storing an encoding that did not shrink buys nothing and costs a decode on every later read of that object, which is the trade the floor exists to refuse.</p><p>On a fleet of media or archives most objects land here. That is a healthy run, not a broken one, which is why a rewrite pass counts these as skipped rather than failed.</p>'
    },
    ADMIT: {
      title: 'Admit on Upload Size',
      badge: 'filter', badgeText: 'quota check',
      body: '<p>Usage admission (<code>EligibleForWrite</code>) runs against <code>plan.uploadSize</code>, the bytes that will actually be uploaded: the encoding, enveloped when encryption is on. The same call also drops backends that are not write targets (cordoned, drain-flagged, or not serving <code>PutObject</code> and <code>GetObject</code>) and those whose <code>max_object_size</code> is below the upload.</p><p>This is the one asymmetry with encryption. An envelope is a header plus a tag per chunk, a fixed function of the size, so a write below <code>min_size</code> is admitted on its enveloped size before its body is buffered. An encoder only reports its output size once it has run, so a write that reaches the encoder cannot be - and admitting it on the logical size would turn away a write that fits.</p><p>That is why buffering, encoding, the ratio check and encryption all run before this point. The per-backend capacity claim happens later, on each upload attempt, in <code>ClaimWriteTarget</code>.</p>'
    },
    ENCRYPT: {
      title: 'Encryption Enabled?',
      badge: 'decision', badgeText: 'layer check',
      body: '<p>Compression runs before encryption, in that order only, because ciphertext does not compress.</p><p>That ordering makes the compressed stream the encryptor\'s input, which is what <code>plaintext_size</code> records - the pre-encryption size, not the object the client wrote. The client\'s own size lives in <code>logical_size</code>.</p><p><a href="../encryption/">Encryption flow diagram &rarr;</a></p>'
    },
    ENVELOPE: {
      title: 'Encrypt the Prepared Body',
      badge: 'process', badgeText: 'envelope',
      body: '<p>The prepared body, encoded or verbatim, is wrapped in an AES-256-GCM envelope once, before the failover loop. An encoded stream is treated exactly as an unencoded body would be: a fresh data key per object, a 32-byte header, and a nonce plus auth tag per encryption chunk. A multipart object reuses the data key wrapped at <code>CreateMultipartUpload</code>, which its parts share.</p><p>The two chunk sizes are independent. Compression frames are <code>compression.chunk_size</code> of logical input; encryption chunks are <code>encryption.chunk_size</code> of the compressed stream.</p>'
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
      title: 'Ranged GET per Needed Frame',
      badge: 'storage', badgeText: 'S3 API call',
      body: '<p>One ranged backend GET per frame the read touches. The seek table read is one speculative 8 KiB fetch of the tail, and frames inside that tail are served from it with no further GET.</p><p>Each is charged its own API call and its own egress, on the bytes that actually left the backend. Charging once per client request would under-report all but the first.</p><p>Read amplification is exactly the sum of these against what the client was served. If it climbs toward the average object size, reads have regressed to fetching whole objects and decoding them - a change nothing else would surface except the backend bill.</p><p class="ac-metric">Metrics: s3o_compression_fetched_bytes_total, s3o_compression_served_bytes_total</p>'
    },
    DECR: {
      title: 'Copy Encrypted?',
      badge: 'decision', badgeText: 'layer check',
      body: '<p>Read order is the reverse of write order: decrypt, then decompress, then slice.</p><p>Because the compressed stream is the encryptor\'s plaintext domain, a compressed-domain range translates into a ciphertext range through the same chunk arithmetic the uncompressed path uses, with no new maths.</p>'
    },
    DECRYPTF: {
      title: 'Decrypt Ciphertext Chunks',
      badge: 'process', badgeText: 'range decryption',
      body: '<p>Whole ciphertext chunks are fetched because each carries its own GCM auth tag, so the bytes crossing the backend link exceed the frame requested - and that is what the egress charge counts.</p><p class="ac-metric">Metric: s3o_encryption_operations_total{op="decrypt_range"}</p>'
    },
    DECODEF: {
      title: 'Decode Frames',
      badge: 'process', badgeText: 'decompression',
      body: '<p>Frames are decoded as the client reads, not up front. Each is independently decodable, which is what makes an entry point every <code>chunk_size</code> possible instead of only at byte zero.</p><p><code>s3o_compression_errors_total{operation="decode"}</code> counts only failures to open the seek table or to seek to the range start, which fail over to the next copy. A frame that fails to decode later, while the body streams, ends the response but is not counted.</p><p class="ac-metric">Metric: s3o_compression_errors_total{operation="decode"}</p>'
    },
    SLICEF: {
      title: 'Slice to Client Range',
      badge: 'process', badgeText: 'range slice',
      body: '<p>The decoded reader is seeked to the range start and limited to its length, and <code>Content-Range</code> is reported over the logical size.</p><p>An unsatisfiable range is served as the whole object, which is what the uncompressed path does with one it cannot translate.</p>'
    },
    PLAINREAD: {
      title: 'Whole-object GET',
      badge: 'storage', badgeText: 'S3 API call',
      body: '<p>A verbatim copy is read with one GET. When it is unencrypted, any client range is passed through to the backend unchanged, since the stored bytes are already in the coordinates the client used.</p><p>When it is encrypted, the range is translated to whole ciphertext chunks, which are fetched and decrypted, and the plaintext is sliced to the requested bytes.</p>'
    },
    SERVE: {
      title: 'Stream to Client',
      badge: 'success', badgeText: 'success',
      body: '<p>The client receives the bytes it wrote, at the size it wrote them, with the ETag and content hash of the logical object. Nothing about the stored form is visible from the S3 API.</p><p>That invariant is what the whole feature is held to: <code>content_hash</code> covers the logical bytes, so the scrubber undoes the stored form before hashing rather than hashing what the backend holds.</p><p><a href="../background-services/">Background services &rarr;</a></p>'
    }
  }
};
</script>

## Legend

| Node | Meaning |
|------|---------|
| <span class="dg-swatch dg-entry"></span> | Entry point |
| <span class="dg-swatch dg-filter"></span> | Threshold / quota filtering |
| <span class="dg-swatch dg-decision"></span> | Decision / branch |
| <span class="dg-swatch dg-process"></span> | Processing step |
| <span class="dg-swatch dg-storage"></span> | Storage / S3 |
| <span class="dg-swatch dg-success"></span> | Success |
| <span class="dg-swatch dg-reject"></span> | Discarded encoding |

## See also

- [Compression reference](../../docs/compression/) - configuration, operations and what to watch
- [Write path](../write-path/) and [read path](../read-path/) - where the codec sits in each
- [Encryption flow](../encryption/) - the layer compression composes with
