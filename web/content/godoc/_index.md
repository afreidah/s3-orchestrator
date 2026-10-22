---
title: "Go API Reference"
seoTitle: "s3-orchestrator Go API reference"
description: "Generated Go package documentation: the object path, the metadata store, the backend abstraction, the transports, and the background workers."
weight: 40
# The generated reference outnumbers the documentation two to one, so it is
# kept out of search results and the sitemap; a crawler spending its budget
# here reaches less of what the site is for. The pages stay followable.
cascade:
  type: docs
  noindex: true
  sitemap:
    disable: true
---

Generated documentation for the Go packages in this project.

## Core

{{< cards >}}
  {{< card link="object/" title="object" icon="database" subtitle="Multi-backend object CRUD with read failover, broadcast reads and location caching." >}}
  {{< card link="infra/" title="infra" icon="server" subtitle="Backend runtime shared by every proxy subpackage: registry, usage limits, timeouts, admission." >}}
  {{< card link="store/" title="store" icon="collection" subtitle="Metadata store with circuit breaker wrapper and role-based interfaces." >}}
  {{< card link="backend/" title="backend" icon="cloud" subtitle="S3-compatible storage client abstraction with circuit breaker wrapper." >}}
  {{< card link="config/" title="config" icon="cog" subtitle="YAML configuration loading, defaults and validation." >}}
  {{< card link="counter/" title="counter" icon="chart-bar" subtitle="Per-backend usage tracking with local atomic and Redis backends." >}}
  {{< card link="encryption/" title="encryption" icon="lock-closed" subtitle="Envelope encryption with AES-256-GCM and pluggable key providers." >}}
  {{< card link="cache/" title="cache" icon="chip" subtitle="In-memory LRU object data cache with size-aware eviction." >}}
  {{< card link="breaker/" title="breaker" icon="lightning-bolt" subtitle="Generic three-state circuit breaker state machine." >}}
{{< /cards >}}

## Transport

{{< cards >}}
  {{< card link="s3api/" title="s3api" icon="globe-alt" subtitle="S3-compatible HTTP API with SigV4 authentication, rate limiting and admission control." >}}
  {{< card link="admin/" title="admin" icon="adjustments" subtitle="Admin API handler for operational control endpoints." >}}
  {{< card link="ui/" title="ui" icon="desktop-computer" subtitle="Built-in web dashboard for storage management." >}}
  {{< card link="auth/" title="auth" icon="key" subtitle="SigV4 request signing, presigned URLs and credential validation." >}}
  {{< card link="provisioning/" title="provisioning" icon="identification" subtitle="Merges the buckets and credentials the config file and the store each declare into one view." >}}
  {{< card link="httputil/" title="httputil" icon="shield-check" subtitle="TLS certificate hot-reload, client IP extraction and login throttle." >}}
{{< /cards >}}

## Background Services

{{< cards >}}
  {{< card link="worker/" title="worker" icon="refresh" subtitle="Background workers for replication, rebalancing, cleanup, scrubbing and reconciliation." >}}
  {{< card link="notify/" title="notify" icon="bell" subtitle="Webhook notification delivery with durable outbox and retry." >}}
  {{< card link="lifecycle/" title="lifecycle" icon="clock" subtitle="Background service manager with panic recovery and graceful shutdown." >}}
{{< /cards >}}

## Observability

{{< cards >}}
  {{< card link="telemetry/" title="telemetry" icon="presentation-chart-line" subtitle="Prometheus metrics, OpenTelemetry tracing and in-memory log buffer." >}}
  {{< card link="audit/" title="audit" icon="clipboard-list" subtitle="Request ID propagation and structured audit logging." >}}
  {{< card link="event/" title="event" icon="speakerphone" subtitle="Notification event type constants and emit hook." >}}
{{< /cards >}}

## Utilities

{{< cards >}}
  {{< card link="bufpool/" title="bufpool" icon="collection" subtitle="Shared sync.Pool byte buffers for streaming I/O." >}}
  {{< card link="syncutil/" title="syncutil" icon="switch-horizontal" subtitle="AtomicConfig[T] and TTLCache[K,V] generic sync primitives." >}}
  {{< card link="workerpool/" title="workerpool" icon="view-grid" subtitle="Generic bounded-concurrency Run[T] and Collect[T,R]." >}}
{{< /cards >}}
