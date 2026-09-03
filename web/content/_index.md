---
title: "Multi-backend S3 proxy with quotas and replication"
description: "One S3 endpoint in front of many S3 backends. Per-backend byte and request quotas, cross-cloud replication, drain, rebalance, and integrity scrubbing."
archetype: "home"
---

<div style="text-align: center; margin-bottom: 1.5rem;">
  <img src="/images/logo.png?v=2" alt="s3-orchestrator" style="max-width: 350px; height: auto;">
</div>

<div class="landing-subheader" style="max-width: 780px; margin: 0 auto; text-align: center;">
An open-source, highly available distributed S3 proxy that decouples your applications from your storage providers. It sits in front of any number of S3-compatible backends - commercial clouds, a local MinIO instance, or both - and unifies them into a single, intelligent pool.
</div>

<div style="text-align: center; margin-top: 1.5rem;">

{{% button href="docs/quickstart/" style="primary" icon="fas fa-rocket" %}}Quickstart{{% /button %}}
{{% button href="docs/" style="primary" icon="fas fa-book" %}}Documentation{{% /button %}}
{{% button href="godoc/" style="primary" icon="fas fa-code" %}}Go API{{% /button %}}
{{% button href="https://github.com/afreidah/s3-orchestrator" style="primary" icon="fab fa-github" %}}GitHub{{% /button %}}

</div>

<hr style="margin-top: 3rem;">

<h1 style="text-align: center; color: #2a9d73;">Your storage should not be a single point of failure</h1>

Most applications talk directly to one S3 backend, which makes them entirely dependent on that cloud's uptime, pricing, features, and security.

Instead of trusting the underlying providers, s3-orchestrator operates as an active, self-healing metadata and orchestration layer inside your own infrastructure. Clients see one endpoint and one namespace; the proxy decides where bytes live, keeps the copies it promised, and keeps working when a provider does not.

<div style="max-width: 620px; margin: 0 auto;">
{{< mermaid >}}
flowchart LR
    C([S3 Clients]):::client --> O[s3-orchestrator<br/>routing - quotas<br/>replication - encryption]:::orch
    O --> B1[(Provider A)]:::backend
    O --> B2[(Provider B)]:::backend
    O --> B3[(MinIO on-prem)]:::backend

    classDef client fill:#6b4c2a,stroke:#d4a05a,color:#fff,font-weight:bold
    classDef orch fill:#7a5a30,stroke:#e8c070,color:#fff,font-weight:bold
    classDef backend fill:#3a2e20,stroke:#c4a35a,color:#e8dfd0
{{< /mermaid >}}
</div>

<hr style="margin-top: 3rem;">

<h2 style="text-align: center; color: #2a9d73;">Adaptive architecture that scales</h2>

It fits your infrastructure, not the other way around. Run it standalone on an embedded SQLite database with local in-memory caching for zero-dependency management, move to PostgreSQL for a robust relational metadata layer, or scale horizontally to many instances. In the horizontal tier, shared usage counters in Redis close the cross-instance blind spot between database flushes, so quotas hold globally without the nodes coordinating directly.

<div style="max-width: 780px; margin: 0 auto;">
{{< mermaid >}}
flowchart LR
    A["<b>Standalone</b><br/>SQLite + in-memory cache<br/>zero external dependencies"]:::tier
    B["<b>Single node</b><br/>PostgreSQL metadata<br/>relational, backed up, queryable"]:::tier
    C["<b>Horizontal</b><br/>PostgreSQL + Redis counters<br/>N instances, no direct coordination"]:::tier
    A --> B --> C

    classDef tier fill:#3a2e20,stroke:#c4a35a,color:#e8dfd0
{{< /mermaid >}}
</div>

<hr style="margin-top: 3rem;">

<h2 style="text-align: center; color: #2a9d73;">Frictionless, zero-downtime migrations</h2>

Changing providers stops being a cutover. Put the proxy in front of your application, point it at the bucket you already have, and import the existing objects into its metadata layer. Add the new provider and raise the replication factor, and the background workers copy everything across while traffic keeps flowing. When the copies are in place, the online drain primitive empties the old bucket so you can delete it from the config - with no application downtime at any step.

<div style="max-width: 760px; margin: 0 auto;">
{{< mermaid >}}
flowchart TD
    S1["<b>1.</b> Point the proxy at your existing bucket<br/>import syncs the objects into its metadata"]:::step
    S2["<b>2.</b> Add the new provider, raise the replication factor<br/>workers copy everything across in the background"]:::step
    S3["<b>3.</b> Online drain empties the old bucket"]:::step
    S4["<b>4.</b> Remove it from the config"]:::done
    S1 --> S2 --> S3 --> S4

    classDef step fill:#3a2e20,stroke:#c4a35a,color:#e8dfd0
    classDef done fill:#7a5a30,stroke:#e8c070,color:#fff,font-weight:bold
{{< /mermaid >}}
</div>

<hr style="margin-top: 3rem;">

<h2 style="text-align: center; color: #2a9d73;">Stays up when your providers do not</h2>

Built with production plumbing - per-backend and per-database circuit breakers, load shedding, and graceful degradation - the proxy stays alive when backends drop. If a provider goes dark, reads transparently fail over to a surviving replica and new writes route to backends that are still healthy.

Given a spare backend to rebuild onto, the replicator restores the target replication factor while the dead one is still down. When it returns, its copies come back with it and the set is briefly larger than you asked for, so an over-replication worker trims it back to the configured factor.

<div style="max-width: 700px; margin: 0 auto;">
{{< mermaid >}}
flowchart LR
    C([S3 Client]):::client --> O[s3-orchestrator<br/>replication factor: 2]:::orch
    O -->|write| B1[(Backend A)]:::down
    O -.->|replicate| B2[(Backend B)]:::backend
    B1 -.->|read fails| O
    O -->|failover read| B2
    O -.->|rebuild 2nd copy| B3[(Backend C)]:::backend

    classDef client fill:#6b4c2a,stroke:#d4a05a,color:#fff,font-weight:bold
    classDef orch fill:#7a5a30,stroke:#e8c070,color:#fff,font-weight:bold
    classDef backend fill:#3a2e20,stroke:#c4a35a,color:#e8dfd0
    classDef down fill:#3a2e20,stroke:#8b3a3a,color:#d4a0a0
{{< /mermaid >}}
</div>

<hr style="margin-top: 3rem;">

<h2 style="text-align: center; color: #2a9d73;">Optional encryption and compression</h2>

Both are opt-in, and both are handled transparently at the proxy when you turn them on. With encryption enabled, every object gets its own data encryption key, wrapped by a master key you supply inline in the config file, from a file on disk, or through HashiCorp Vault Transit for HSM-backed key management. With compression enabled, objects are stored as chunked zstd.

Enable both and compression runs first, because ciphertext does not compress. Your providers then hold only compressed ciphertext they cannot read, and the objects crossing the wire are smaller - cutting the storage footprint and the transfer fees at the same time.

Object tagging is handled the same way, in the metadata layer rather than pushed down to the providers. Tags stay consistent across every backend even when one lacks tagging support or implements it differently, and every replica of an object shares one set.

<hr style="margin-top: 3rem;">

<h2 style="text-align: center; color: #2a9d73;">Key Features</h2>

<h3 style="color: #a7d5c1;">Storage and routing</h3>

<div class="feature-grid">
  <div class="feature-item">
    <i class="fas fa-layer-group feature-icon" style="color: #2a9d73;"></i>
    <div>
      <strong>Multi-Backend Storage</strong>
      <p>Stack allocations from any number of providers into one logical target.</p>
    </div>
    <div class="feature-detail">Combine capacity from OCI Object Storage, Backblaze B2, AWS S3, MinIO, Wasabi, Cloudflare R2, or any S3-compatible provider. The orchestrator routes writes based on available quota and presents every backend as one unified endpoint.</div>
  </div>
  <div class="feature-item">
    <i class="fas fa-tachometer-alt feature-icon" style="color: #7dd3fc;"></i>
    <div>
      <strong>Per-Backend Quotas</strong>
      <p>Cap each backend at an exact byte limit to avoid surprise bills.</p>
    </div>
    <div class="feature-detail">Set a byte limit on each backend and the orchestrator enforces it atomically on every write. When a backend fills up, writes overflow to the next available backend automatically. Limits are hot-reloadable, and a quota of 0 disables enforcement.</div>
  </div>
  <div class="feature-item">
    <i class="fas fa-traffic-light feature-icon" style="color: #fca5a5;"></i>
    <div>
      <strong>Usage Limits</strong>
      <p>Cap monthly API requests, egress, and ingress per backend.</p>
    </div>
    <div class="feature-detail">Set monthly caps on API requests, egress bytes, and ingress bytes per backend, reset automatically each month. When a backend exceeds a limit, writes overflow to other backends and reads fail over to replicas. Adaptive flushing shortens the tracking interval as limits approach, and Redis-backed shared counters keep enforcement tight across multiple instances.</div>
  </div>
  <div class="feature-item">
    <i class="fas fa-boxes feature-icon" style="color: #6ee7b7;"></i>
    <div>
      <strong>Virtual Buckets</strong>
      <p>Isolated namespaces and independent credentials per application.</p>
    </div>
    <div class="feature-detail">Each bucket has its own SigV4 access key and secret key, with support for presigned URLs (up to 7-day expiry) for direct browser uploads and downloads. Objects are stored with an internal key prefix, so bucket isolation requires zero changes to the storage layer or database schema.</div>
  </div>
</div>

<h3 style="color: #a7d5c1;">Durability and data protection</h3>

<div class="feature-grid">
  <div class="feature-item">
    <i class="fas fa-sync feature-icon" style="color: #fca5a5;"></i>
    <div>
      <strong>Cross-Backend Replication</strong>
      <p>Multi-cloud redundancy with zero client-side changes.</p>
    </div>
    <div class="feature-detail">Set a replication factor and a background worker ensures every object exists on that many backends. Objects are written to one backend on PUT; the replicator asynchronously copies them to reach the target factor, and an over-replication worker removes copies beyond it.</div>
  </div>
  <div class="feature-item">
    <i class="fas fa-lock feature-icon" style="color: #c4b5fd;"></i>
    <div>
      <strong>Server-Side Encryption</strong>
      <p>Envelope encryption with AES-256-GCM via inline keys, files, or Vault Transit.</p>
    </div>
    <div class="feature-detail">Each object gets a unique data encryption key (DEK), wrapped by the master key. Supports inline config keys, file-based keys, or HashiCorp Vault Transit for HSM-backed key management. Key rotation re-wraps DEKs without touching object data.</div>
  </div>
  <div class="feature-item">
    <i class="fas fa-file-zipper feature-icon" style="color: #f0883e;"></i>
    <div>
      <strong>At-Rest Compression</strong>
      <p>Chunked zstd, so a range read stays a range read.</p>
    </div>
    <div class="feature-detail">Objects are stored as chunked zstd in the Zstandard seekable format: one independently decodable frame per chunk, with a seek table. That keeps partial reads proportional to the bytes requested rather than to object size, which matters when backends meter egress. Sizes, ETags and content hashes remain those of the object the client wrote.</div>
  </div>
  <div class="feature-item">
    <i class="fas fa-binoculars feature-icon" style="color: #6ee7b7;"></i>
    <div>
      <strong>Continuous Integrity Scrubbing</strong>
      <p>Finds silent corruption and missing replicas out of band.</p>
    </div>
    <div class="feature-detail">A background scrubber continuously validates stored bytes against recorded content hashes, catching degradation without impacting client-facing performance. A copy that fails is discarded and rebuilt from a good replica. Content-hash backfill brings objects written before hashing under the same protection.</div>
  </div>
</div>

<h3 style="color: #a7d5c1;">Operations</h3>

<div class="feature-grid">
  <div class="feature-item">
    <i class="fas fa-truck-moving feature-icon" style="color: #e2b07a;"></i>
    <div>
      <strong>Zero-Downtime Migration</strong>
      <p>Import an existing bucket, replicate to the new one, drain the old.</p>
    </div>
    <div class="feature-detail">Native import brings the objects already in a bucket under management without moving them. Raising the replication factor copies them to a new provider in the background, and the online drain primitive then empties the old backend so it can be removed from the config. Every step happens while traffic continues.</div>
  </div>
  <div class="feature-item">
    <i class="fas fa-shield-alt feature-icon" style="color: #fca5a5;"></i>
    <div>
      <strong>Resilience Plumbing</strong>
      <p>Circuit breakers, load shedding, and graceful degradation.</p>
    </div>
    <div class="feature-detail">Per-backend and per-database circuit breakers open on sustained failure and probe for recovery. Admission control sheds load with 503 SlowDown rather than collapsing under a burst, and a database outage degrades to bounded broadcast reads instead of taking the proxy down.</div>
  </div>
  <div class="feature-item">
    <i class="fas fa-clock feature-icon" style="color: #6ee7b7;"></i>
    <div>
      <strong>Lifecycle Management</strong>
      <p>Automatic object expiration with configurable rules.</p>
    </div>
    <div class="feature-detail">Define expiration rules that target specific key prefixes - for example, cleaning up temporary uploads or cache objects after a set period. Only objects matching the configured prefix patterns are expired; everything else is left untouched. A background worker deletes both the backend storage and the database metadata.</div>
  </div>
  <div class="feature-item">
    <i class="fas fa-bolt feature-icon" style="color: #7dd3fc;"></i>
    <div>
      <strong>Object Data Cache</strong>
      <p>In-memory LRU cache for read-heavy workloads.</p>
    </div>
    <div class="feature-detail">Optional in-memory LRU cache that serves repeated reads locally, reducing backend API calls and egress. Configurable maximum size, per-object size limit, and TTL. Automatically invalidated on writes and deletes so it cannot serve stale bytes.</div>
  </div>
</div>

<h3 style="color: #a7d5c1;">Interfaces and observability</h3>

<div class="feature-grid">
  <div class="feature-item">
    <i class="fas fa-plug feature-icon" style="color: #2a9d73;"></i>
    <div>
      <strong>S3-Compatible API</strong>
      <p>Works with aws cli, rclone, Terraform, and any standard S3 SDK.</p>
    </div>
    <div class="feature-detail">Supports PutObject, GetObject, HeadObject, DeleteObject, CopyObject, ListObjectsV2, multipart uploads, range reads, conditional writes, and user metadata. Any tool that speaks S3 works with no code changes.</div>
  </div>
  <div class="feature-item">
    <i class="fas fa-tags feature-icon" style="color: #58a6ff;"></i>
    <div>
      <strong>Provider-Agnostic Tagging</strong>
      <p>Object tags held in the metadata layer, consistent across every backend.</p>
    </div>
    <div class="feature-detail">Tags are stored and managed in your own database rather than pushed to the providers, so they stay consistent even when a backend lacks tagging support or implements it differently - and a backend sitting over its usage limit can still be tagged. One set per object, shared by every replica, reachable from the S3 API, the admin API, the CLI, and the TUI.</div>
  </div>
  <div class="feature-item">
    <i class="fas fa-chart-bar feature-icon" style="color: #7dd3fc;"></i>
    <div>
      <strong>Web Dashboard</strong>
      <p>Real-time storage overview, directory browser, and admin operations.</p>
    </div>
    <div class="feature-detail">Built-in web UI with storage summaries, per-backend quota bars, monthly usage charts, a lazy-loaded directory tree for browsing and deleting objects, and admin controls for rebalancing, syncing, and uploading.</div>
  </div>
  <div class="feature-item">
    <i class="fas fa-terminal feature-icon" style="color: #a78bfa;"></i>
    <div>
      <strong>Terminal Object Browser</strong>
      <p>Full-screen TUI to browse objects and inspect replica placement.</p>
    </div>
    <div class="feature-detail">A full-screen terminal UI (<code>s3-orchestrator tui</code>) that browses the object namespace one prefix at a time and opens an inspector on any object showing every backend copy - backend, size, age, stored form, encryption status, key id, content hash, and tag set. Resolves its admin target exactly like the admin CLI.</div>
  </div>
  <div class="feature-item">
    <i class="fas fa-fire feature-icon" style="color: #fca5a5;"></i>
    <div>
      <strong>Observability</strong>
      <p>Prometheus metrics, OpenTelemetry tracing, structured audit logging.</p>
    </div>
    <div class="feature-detail">Exposes Prometheus metrics for all operations, quotas, and background tasks, and ships with a pre-built Grafana dashboard covering request rates, latency, backend health, quota usage, and replication status. OpenTelemetry tracing with configurable sampling. Structured JSON audit logs with request ID correlation across HTTP and storage layers.</div>
  </div>
  <div class="feature-item">
    <i class="fas fa-cubes feature-icon" style="color: #e8c070;"></i>
    <div>
      <strong>Deploy Anywhere</strong>
      <p>A single binary, a systemd service, or a container - your choice.</p>
    </div>
    <div class="feature-detail">Runs as a plain binary, as a systemd service from the Debian package (which ships its own unit file), or as a container image with ready-made Nomad HCL and Kubernetes manifests. Nothing about the design assumes an orchestrator. Release artifacts are signed, and configuration is hot-reloadable on SIGHUP, so quotas, backends, and limits change without a restart.</div>
  </div>
</div>

<hr style="margin-top: 3rem;">

<h2 style="text-align: center; color: #2a9d73;">Who Is This For?</h2>

<div class="feature-grid">
  <div class="feature-item">
    <i class="fas fa-shield-alt feature-icon" style="color: #c4b5fd;"></i>
    <div>
      <strong>Anyone Who Wants Provider Independence</strong>
      <p>Avoid vendor lock-in. Your applications talk S3 to one endpoint - swap, add, or remove backends without touching a line of code.</p>
    </div>
  </div>
  <div class="feature-item">
    <i class="fas fa-building feature-icon" style="color: #e2b07a;"></i>
    <div>
      <strong>Teams Migrating Between Providers</strong>
      <p>Import an existing bucket, replicate to the new provider in the background, and drain the old one - without a cutover window.</p>
    </div>
  </div>
  <div class="feature-item">
    <i class="fas fa-hdd feature-icon" style="color: #6ee7b7;"></i>
    <div>
      <strong>Self-Hosters Running MinIO</strong>
      <p>Add automatic cloud backups to a local MinIO instance with one config change - no sync scripts or extra tooling.</p>
    </div>
  </div>
  <div class="feature-item">
    <i class="fas fa-user-cog feature-icon" style="color: #2a9d73;"></i>
    <div>
      <strong>Homelabbers</strong>
      <p>Stack free-tier allocations from several providers into usable storage, with hard caps on transit so there is no surprise bill. The <a href="guides/maximizing-free-tiers/">free-tier guide</a> walks through ten providers and what each one meters.</p>
    </div>
  </div>
</div>

<hr style="margin-top: 3rem;">

<h2 style="text-align: center; color: #2a9d73;">Admin Web Interface</h2>

A built-in web dashboard provides real-time storage summaries, per-backend quota and usage bars, monthly traffic charts, a lazy-loaded directory tree for browsing and managing objects, and admin controls for rebalancing, syncing, uploading, and deleting files and folders.

![Admin Web Interface](/docs/images/admin-ui.png)

<hr style="margin-top: 3rem;">

<h2 style="text-align: center; color: #2a9d73;">Built-in Monitoring</h2>

s3-orchestrator ships with a pre-built Grafana dashboard and Prometheus metrics out of the box. Track request rates, latency percentiles, backend health, quota usage, replication progress, and background task performance - all without writing a single query.

![Grafana Dashboard](images/grafana.png)

<hr style="margin-top: 3rem;">

<div class="landing-subheader" style="max-width: 760px; margin: 0 auto 3rem; text-align: center;">
The data durability, privacy, and operational primitives of a massive enterprise storage system - deployed as a single lightweight service inside the infrastructure you are already running.
</div>
