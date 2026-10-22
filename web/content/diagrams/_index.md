---
title: "Diagrams"
seoTitle: "s3-orchestrator diagrams"
description: "Interactive diagrams of the request paths, storage layer, background workers, and database schema, with per-component implementation notes."
weight: 35
cascade:
  type: docs
---

Interactive architecture and flow diagrams of the orchestrator's internals. Hover over any component for implementation details.

{{< cards >}}
  {{< card link="architecture/" title="System Architecture" icon="template" subtitle="The request path, storage layer, background services and observability, end to end." >}}
  {{< card link="admission-control/" title="Admission Control Flow" icon="filter" subtitle="A request through admission control, authentication, routing and circuit breakers." >}}
  {{< card link="access-control/" title="Access Control Flow" icon="shield-check" subtitle="How a credential resolves to a user, and how that user's grants authorize the S3 API, the admin API and the dashboard." >}}
  {{< card link="write-path/" title="Write Path" icon="upload" subtitle="PutObject through backend selection, compression, encryption, failover and metadata recording." >}}
  {{< card link="read-path/" title="Read Path" icon="download" subtitle="GetObject through location lookup, failover, broadcast reads, decryption, frame decoding and streaming." >}}
  {{< card link="circuit-breaker/" title="Circuit Breaker" icon="lightning-bolt" subtitle="Breaker states for the database, Redis counters, each backend and each backend call." >}}
  {{< card link="encryption/" title="Encryption Flow" icon="lock-closed" subtitle="DEK generation, key wrapping, chunked AES-256-GCM and range decryption." >}}
  {{< card link="compression/" title="Compression" icon="archive" subtitle="Chunked zstd: size and ratio floors, seek table, frame-level ranged reads, and composition with encryption." >}}
  {{< card link="tagging/" title="Object Tagging" icon="tag" subtitle="The four ways a tag set is written, validation and the per-key lock, and what clears it." >}}
  {{< card link="usage-quotas/" title="Usage Quotas" icon="scale" subtitle="Per-operation request budgets, pooled admission, unmetered operations and where the counters live." >}}
  {{< card link="background-services/" title="Background Services" icon="cog" subtitle="Every periodic worker, from the replicator and rebalancer to the scrubber and usage flusher." >}}
  {{< card link="database-schema/" title="Database Schema" icon="database" subtitle="The PostgreSQL metadata store, which SQLite mirrors: tables, columns, indexes and relationships." >}}
{{< /cards >}}
