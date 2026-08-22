---
title: "Compression"
linkTitle: "Compression"
weight: 28
---

# Compression

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

PUT compresses, and GET, HEAD and server-side copy all serve the object the client wrote, at the size it wrote. Multipart uploads and the background workers do not handle the compressed form yet: the scrubber hashes stored bytes without decoding them, so it reads a compressed object as corrupt. Leave `enabled: false` until those land.

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
