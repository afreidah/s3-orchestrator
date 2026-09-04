---
description: "At-rest chunked zstd compression: what changes on the backend, what stays identical to the object the client wrote, and how to enable it."
title: "Compression"
linkTitle: "Compression"
weight: 28
---

At-rest compression. When enabled, objects are stored on backends as chunked zstd; sizes, ETags and content hashes stay those of the object the client wrote. Disabled by default.

Storage and transfer are both metered on the backends this project targets, so compression reduces the bill twice. That second saving only holds if a partial read stays cheap, which is what drives the format.

```yaml
compression:
  enabled: true
  level: "default"             # fastest, default, better, or best
  chunk_size: 1048576          # default: 1MB (range: 16KB-64MB)
  min_size: 4096               # objects smaller than this are stored uncompressed
  min_ratio: 0.95              # encoded objects above this fraction of the original are discarded
```

## Current status

Every path handles compression: PUT and multipart completion write it, GET and HEAD serve through it, server-side copy and the replication workers carry it, the scrubber decodes before hashing, reconcile recognises it, and a crash-recovered write keeps it. Still off by default, and still worth enabling on a test fleet before a production one.

## Seeing it per object and per backend

The object inspector shows how each copy is stored: a `COMP` column naming the encoding, and a `LOGICAL` column with the size the client wrote. Both read as a dash for a copy stored verbatim, where the stored size already is the object's size. That is the view that answers "is this particular object compressed, and by how much" - the copies of one key can differ, since a rewrite pass works one copy at a time.

The dashboard and the TUI backends pane carry a `Saved` column per backend: what the compressed objects on it are, less what they occupy. The dashboard also has a Compression Savings section with the fleet totals and the ratio achieved. That ratio covers only objects stored encoded, so it is what the encoder managed rather than an average diluted by everything it declined.

## What to watch

Two ratios say whether the feature is doing its job, and both are a pair of counters rather than a single figure so nothing has to be kept in step:

| Question | Expression |
|----------|------------|
| What ratio is the fleet achieving? | `s3o_compression_stored_bytes_total / s3o_compression_logical_bytes_total` |
| How many bytes has it saved? | `s3o_compression_logical_bytes_total - s3o_compression_stored_bytes_total` |
| Are ranged reads still ranged? | `s3o_compression_fetched_bytes_total / s3o_compression_served_bytes_total` |

The last one is read amplification, and it is the number that proves the chunked format is earning its ratio penalty. A range read fetches the frames it covers, so the figure is bounded by the chunk size against the ranges clients ask for. If it climbs towards the average object size, reads have regressed to fetching whole objects and decoding them - a change nothing else would surface except the backend bill.

`s3o_compression_skipped_total` separates the two floors, which matters when a fleet compresses less than expected: `min_size` means the objects are small, `min_ratio` means they are already compressed, and the two call for different responses. `s3o_compression_ratio` gives the per-object distribution, since a fleet averaging 0.4 behaves differently depending on whether that is every object or half of them at 0.05.

`s3o_compression_errors_total{operation="decode"}` deserves an alert on any value. An encode failure fails one write; a decode failure means bytes already stored cannot be read back.

The Grafana dashboard carries all of these in a Compression row.

## Bringing an existing fleet under compression

Enabling `compression` affects objects written afterwards. Everything already stored stays exactly as it is until something rewrites it, which is what `compress-existing` does:

```bash
s3-orchestrator admin compress-existing
s3-orchestrator admin decompress-existing   # and back out again
```

Both are available in the web UI as well, and as `POST /admin/api/compress-existing` and `/admin/api/decompress-existing`.

The pass reads each copy, encodes it, writes it back in place and updates the row, moving the backend's quota by the difference in one transaction with it. `min_size` and `min_ratio` apply exactly as they do to a PUT, so the pass cannot store an encoding a fresh write would have rejected.

Objects it declines are reported as **skipped**, separately from failures. On a fleet of media or archives most of it will be skipped, and that is a healthy run rather than a broken one. Only objects below `min_size` are free to skip: deciding on the ratio means encoding the object first, since nothing else answers the question.

### Converting in batches

`-max` stops the pass after that many objects, which is how a fleet-sized conversion is spread across maintenance windows rather than run in one sitting:

```bash
s3-orchestrator admin compress-existing -max=1000
```

The same cap is a `max` query parameter on both endpoints, and the TUI offers each pass twice, once whole-fleet and once as a batch that asks for a count. Without it, both convert everything, which is the default on every entry point.

Nothing has to be carried between runs, and nothing needs tracking on your side. A converted copy no longer matches the listing that selected it, and one declined on `min_ratio` has what the encoder produced recorded against its row, so it drops out of the listing too. Run the command again a week later and it picks up the objects the previous runs did not reach, including anything written in the meantime.

That recording is what keeps a batched conversion from costing more than a single pass. Proving an object incompressible means downloading and encoding it, so a pass that forgot the answer would spend that egress again on every subsequent run - on a metered backend, quota spent to reach a verdict already reached. The stored measurement is judged against the current `min_ratio` rather than treated as final, so lowering the threshold returns those objects to the pass with no read at all. Changing `level` has the same effect, since a measurement taken at another level says nothing about the current one.

An encrypted object is decrypted, encoded, and re-encrypted, because compression sits inside encryption. That mints a fresh data key for the rewritten copy - re-encrypting changes the base nonce whatever key is used, so keeping the old one preserves nothing, and the new one is wrapped under whichever key is primary now.

The pass rewrites one copy at a time, so a replicated object is brought over one copy per pass rather than atomically. Copies that differ in stored form are not a problem: every row describes its own bytes.

## Why the format is chunked

Compression emits backreferences into earlier data, so decoding can only start at a frame boundary. One frame per object gives exactly one entry point, byte zero, and any range read then has to fetch the whole stored object and discard everything before the offset. The cost of a partial read becomes proportional to object size rather than to the bytes asked for, which is the wrong trade for a proxy whose backends meter egress.

Range reads are not an edge case: resumed downloads, query engines splitting a file across workers, and index-at-the-end formats like ZIP and Parquet all depend on them.

Objects are therefore written in the Zstandard seekable format, one independently decodable frame per chunk with a seek table in a trailing skippable frame. That gives one entry point per chunk, at the cost of an index and a small ratio penalty.

## Choosing a chunk size

The ratio penalty for splitting an object into frames, against one frame per object. Negative means the chunked form was smaller.

| Chunk size | Go source | JSON logs |
|-----------|-----------|-----------|
| 64 KiB | +17.2% | -1.2% |
| 256 KiB | +7.5% | -2.1% |
| 1 MiB | +2.5% | -1.5% |
| 4 MiB | +0.0% | -0.5% |

At 1 MiB the ratio cost is negligible and throughput is unchanged, which is why that is the default. Smaller chunks make a small range read cheaper and cost ratio; larger chunks do the reverse, since serving one byte still means fetching and decoding a whole frame.

The chunk size is fixed for the data it writes. Changing it affects new objects only: every existing object carries its own layout in its own seek table and stays readable.

## Why the level is a name and not a number

zstd collapses its numeric 1-19 range into four buckets, so levels 10 and 19 produce byte-identical output. A numeric setting would let an operator express a distinction the encoder discards.

| Level | Ratio (Go source) | Compress | Decompress |
|-------|-------------------|----------|------------|
| `fastest` | 0.227 | 201 MB/s | 621 MB/s |
| `default` | 0.205 | 177 MB/s | 517 MB/s |
| `better` | 0.186 | 143 MB/s | 874 MB/s |
| `best` | 0.173 | 22 MB/s | 595 MB/s |

Decompression speed does not degrade with level; compression speed collapses at the top. The trade is entirely on the write side, so `default` is the default.

## Composition with encryption

Compression runs before encryption, in that order only, because ciphertext does not compress. A read runs it backwards: decrypt, then decompress, then slice.

That ordering is what makes the compressed stream the encryptor's plaintext domain, and it is why an object with both features on carries two sizes:

| Column | Holds |
|--------|-------|
| `size_bytes` | What the backend stores. Ciphertext of compressed data when both are on. |
| `plaintext_size` | The encryptor's input, which is the compressed stream. |
| `logical_size` | The object the client wrote. |

With compression off, `logical_size` is unset and `plaintext_size` is the object's own size, exactly as before.

## What a stored object records

Four nullable columns on `object_locations` and `pending_objects`:

| Column | Purpose |
|--------|---------|
| `compression_algorithm` | What a decoder dispatches on. NULL means the bytes are stored verbatim, so no separate boolean can drift out of step with it. |
| `compression_level` | What the object was written at. Diagnostic, and what a rewrite pass reads; decoding does not need it. |
| `compression_format_version` | The on-disk layout version, so a later change is detectable rather than silently misread. |
| `logical_size` | The size the client wrote, needed to size a response and bound range math. |

Every row that predates the feature has a NULL algorithm and is therefore correctly described as verbatim, so no backfill is required.

## Multipart uploads

An object uploaded in parts is compressed once, when the parts are assembled, not part by part as they arrive. Its chunk layout is therefore the same as any other object's and owes nothing to the part sizes the client chose, which matters because those are arbitrary: a client picking 8 MiB parts and one picking 500 MiB parts produce identically seekable objects.

Assembly already rewrites the object rather than concatenating stored bytes - encrypted parts are decrypted as they stream so the assembled object is a single envelope - so compression costs no extra pass over the data. It does mean the encoded object is buffered before it is sent, because a backend PUT declares its size up front and an encoder only knows that size once it has finished. Objects above 32 MiB spill to a temporary file rather than being held in memory.

`min_ratio` applies here as it does to a single PUT. The parts stream past exactly once, so an encoding that fails to earn its place is decoded back out of that same buffer rather than re-read from the backend, which would cost a second egress charge on every part.

## Copies made by the workers

Replication, rebalance and drain move stored bytes verbatim - they never decode - so the row each one writes for the new copy repeats what the source row said. That description is produced by one conversion shared with the write and copy paths, rather than by each path listing the columns it happens to remember.

A copy recorded without it is not a degraded copy but an unreadable one: the bytes are chunked zstd and the row says they are not, so the read path serves them raw at the wrong size. The replicator would then spread that row to further backends.

## Objects rediscovered on a backend

Reconcile and `sync` are the only paths that start from bytes rather than from a client request, so they are the only place the orchestrator has to work out how an object is stored instead of being told.

An encoding is recognised by its seek table. The frame magic cannot decide it: the stored form is a valid Zstandard stream on purpose, so the magic matches a `.zst` file a client uploaded just as well, and decoding one of those on read would return bytes the client never sent. A plain zstd encoder writes no seek table, which is what separates the two.

Recognised objects are imported with the logical size their seek table declares, which is the number nothing else could supply for an object whose rows are all gone. Anything unrecognised is imported as the verbatim bytes it appears to be.

A compressed object that is also encrypted cannot be recognised from outside, since it was compressed before it was encrypted. Those inherit their description from a surviving copy's row instead, along with the key.

## Quota and recovery

Quota counts what occupies a backend, so every size the write path records is the stored size: the encoded, encrypted bytes that landed, not the object they decode to. `logical_size` exists to answer the client and never to charge the operator.

That holds through failure too. A PUT records its intent before the upload, so a crash between the two leaves a row the reaper resolves later; the intent carries the same description of the bytes the commit would have, so a recovered object is decodable rather than merely present. A write whose commit fails leaves a cleanup row sized by what landed, which is what credits the backend back when the orphan is removed.

## Verification

`content_hash` covers the bytes the client wrote, so the scrubber undoes the stored form before hashing: decrypt, then decompress, the reverse of the order they were applied in. Anything else writes a digest of the wrong bytes, which every later verification reads as corruption.

It decodes front to back rather than through the seek table, since it reads the whole object anyway and would otherwise have to buffer it locally to have something seekable.

A copy this orchestrator cannot decode - a compressed object with no codec configured, or bytes the codec rejects - is reported as unreadable rather than corrupt, and is left alone. The distinction matters because the scrubber deletes what it judges corrupt, and a copy it could not read has not been judged.

## Copying an object

A server-side copy moves the stored bytes as they are and writes the source's representation metadata onto the destination. Nothing is decoded or re-encoded, so a copy stays a metadata operation and an object stored verbatim stays verbatim.

The destination therefore inherits the chunk size and level its source was written at rather than the ones configured now. A change to either reaches an existing object only when something rewrites that object.

## Skipping objects that will not benefit

Two thresholds, because an object can fail to benefit for two different reasons.

`min_size` stores small objects verbatim. A seek table and per-frame headers cost more than a small object saves, and the floor avoids paying that for no return.

`min_ratio` handles the other reason: entropy rather than size. Random data compresses to a ratio of exactly 1.000, so already-compressed content, media and archives gain nothing from a second pass, at any size. An object that encodes to more than `min_ratio` of its original size is stored as the client sent it, and its row carries no algorithm - the same way a small object's does. Nothing distinguishes the two cases on read, because nothing needs to.

The decision is made by encoding the object and measuring the result, not by sampling its first chunk. Entropy is not uniform across an object, so a sample can be wrong in the direction that costs bytes for the whole life of that object, whereas encoding an object that turns out to be incompressible costs only the encode. That is the cheapest case the encoder has: zstd detects blocks it cannot shrink and stores them raw, which runs about five times faster than compressing data it can shrink - 1360 MB/s against 245 MB/s on structured text it takes to a ratio of 0.19. `BenchmarkCompressIncompressible` and `BenchmarkCompressLogLike` in `internal/compression` are those measurements; `make bench-compression` runs them.

## Interoperability

A stored object is a valid Zstandard stream. The seek table lives in a skippable frame, which conforming decoders ignore, so `zstd -d` decodes an object the orchestrator wrote without knowing anything about the seek table. That matters for recovery: the bytes on a backend are readable without this software.

## See also

- [Configuration reference](configuration.md#compression) for every field and its bounds
- [Encryption](encryption.md) for the layer compression composes with
- [Architecture](architecture.md) for where the codec sits in the write path
