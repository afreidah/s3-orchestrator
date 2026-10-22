---
title: "s3-orchestrator"
description: "Storage orchestration layer presenting many S3 providers as one S3 endpoint: copies across providers with read failover, per-backend quotas and usage limits, drain, rebalance, and integrity scrubbing."
layout: hextra-home
# Roots the sidebar at the site, so docs, guides, diagrams and the Go API all
# show in it rather than only the section a page is in.
type: docs
---

<div class="s3o-hero hx:w-full hx:flex hx:flex-col hx:items-center hx:text-center">

<img class="s3o-hero-logo" src="images/logo.png" alt="s3-orchestrator" width="350" height="350" fetchpriority="high">

<div class="hx:mt-8 hx:mb-6">
{{< hextra/hero-headline >}}
  A storage orchestration layer&nbsp;<br class="hx:sm:block hx:hidden" />across your S3 providers
{{< /hextra/hero-headline >}}
</div>

<p class="hx:mb-6 hx:text-lg hx:font-semibold">Free and self-hosted. No account, no subscription, no middleman.</p>

<div class="hx:mb-10">
{{< hextra/hero-subtitle style="margin: 0 auto;" >}}
  Presents any number of S3-compatible providers as one S3 endpoint and tracks where every object lives in its own database.&nbsp;<br class="hx:sm:block hx:hidden" />From that ledger it places each write, keeps the copies you configured across providers, fails reads over, enforces per-provider limits, and moves data between providers while traffic continues.
{{< /hextra/hero-subtitle >}}
</div>

<div class="hx:flex hx:flex-wrap hx:gap-3 hx:justify-center hx:mb-6">
{{< hextra/hero-button text="Quickstart" link="docs/quickstart" >}}
{{< hextra/hero-button text="Documentation" link="docs" style="background: transparent; border: 1px solid var(--s3o-border); color: var(--s3o-text);" >}}
{{< hextra/hero-button text="GitHub" link="https://github.com/afreidah/s3-orchestrator" style="background: transparent; border: 1px solid var(--s3o-border); color: var(--s3o-text);" >}}
</div>

</div>

<div class="s3o-diagram">
<img src="images/01-unified-storage.svg" width="1200" height="520" alt="S3 clients reach one endpoint on the orchestrator, which fans writes out to Provider A, Provider B, and an on-prem MinIO.">
</div>

<div class="s3o-sections">

<div class="s3o-row">
<div class="s3o-text">

## Quotas and usage limits

Each backend gets a byte quota, enforced atomically on every write, and
optional monthly caps on egress, ingress and API requests. Request budgets are
named pools over the operations a provider bills together. A backend that
reaches a limit is skipped: writes overflow to the next eligible backend and
reads fail over to a replica.

<a class="s3o-more" href="docs/backends/">Backends &rarr;</a>

</div>
<div class="s3o-example">

```yaml
backends:
  - name: "gcs"
    quota_bytes: 5368709120          # 5 GB
    egress_byte_limit: 1073741824    # 1 GB a month
    unmetered: [DeleteObject, DeleteObjects, AbortMultipartUpload]
    request_limits:
      - name: class_a
        operations: [PutObject, CopyObject, CreateMultipartUpload, UploadPart,
                     CompleteMultipartUpload, ListObjects, ListObjectsV2]
        limit: 5000
      - name: class_b
        operations: [GetObject, HeadObject, GetParts]
        limit: 50000
```

</div>
</div>

<div class="s3o-row s3o-flip">
<div class="s3o-text">

## Replication and failover

Set a replication factor and every object ends up on that many backends,
either copied by a background replicator or placed by the write itself. Reads
fail over to a surviving copy when a backend goes down, the replicator
rebuilds onto a spare, and an over-replication worker trims the extra copy
when the backend returns.

<a class="s3o-more" href="docs/replication/">Replication &rarr;</a>

</div>
<div class="s3o-example">

<img src="images/04-provider-failover.svg" width="1200" height="560" alt="With replication factor 2 and one backend down, the orchestrator serves the surviving copy and replicates back when the backend returns.">

</div>
</div>

<div class="s3o-row">
<div class="s3o-text">

## Migrations without downtime

Point the proxy at the bucket you already have and import its objects into
the metadata layer without moving them. Add the new provider and raise the
replication factor, and the workers copy everything across while traffic
continues. Then drain the old backend online and remove it from the config.

<a class="s3o-more" href="docs/operations/">Operations &rarr;</a>

</div>
<div class="s3o-example">

<img src="images/03-zero-downtime-migration.svg" width="1200" height="520" alt="Migration in three steps: point the proxy at the existing bucket, raise the replication factor so workers copy across, then drain the old bucket online.">

</div>
</div>

<div class="s3o-row s3o-flip">
<div class="s3o-text">

## Operations from the terminal

The admin CLI runs every maintenance pass, from reconcile and replicate to
scrub, drain and rebalance, and reports each item as it goes. The TUI shows
per-backend usage, health and backlogs, and browses the object namespace down
to where each replica lives.

<a class="s3o-more" href="docs/cli/">CLI &rarr;</a>

</div>
<div class="s3o-example">

```text
$ s3-orchestrator admin reconcile
reconcile started
  reconciling aws-east ........................... OK     (335ms)
  reconciling backblaze .......................... OK     (510ms)
  reconciling wasabi ............................. OK     (162ms)
  reconciling minio .............................. OK     (261ms)
done: imported 32, removed 4 across 4 backend(s) (1.3s)
```

</div>
</div>

<div class="s3o-row">
<div class="s3o-text">

## From one node to many

Run standalone on SQLite with an in-memory cache, move the metadata to
PostgreSQL, or run many instances behind a load balancer. With several
instances, shared usage counters in Redis keep quotas and limits global
between database flushes.

<a class="s3o-more" href="docs/deployment/">Deployment &rarr;</a>

</div>
<div class="s3o-example">

<img src="images/02-adaptive-architecture.svg" width="1200" height="500" alt="Three deployment tiers: standalone on SQLite, a single node on PostgreSQL, and horizontal scaling with PostgreSQL plus Redis counters.">

</div>
</div>

</div>

<div class="s3o-more-list">

<div>

### Encryption and compression

Envelope encryption with a key per object, wrapped by an inline, file or Vault
Transit master key. Chunked zstd keeps ranged reads proportional to the bytes
requested. With both on, objects are compressed before they are encrypted.

</div>
<div>

### Integrity scrubbing

Content hashes recorded on write and checked by a background scrubber. A copy
that no longer matches is discarded and rebuilt from a healthy replica, and
the scrubber leaves a share of each backend's egress limit for client reads.

</div>
<div>

### Tagging in the metadata layer

Tag sets live in the orchestrator's database, one per object and shared by
every replica, so they stay consistent on backends that lack tagging or are
over their usage limits.

</div>

</div>
