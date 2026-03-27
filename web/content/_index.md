---
title: " "
archetype: "home"
---

<div style="text-align: center; margin-bottom: 1.5rem;">
  <img src="/images/logo.png?v=2" alt="s3-orchestrator" style="max-width: 350px; height: auto;">
</div>

<div class="badge-grid">

{{% badge style="primary" icon="fas fa-cloud" %}}S3-Compatible{{% /badge %}}
{{% badge style="info" title=" " icon="fas fa-shield-alt" %}}Envelope Encryption{{% /badge %}}
{{% badge style="danger" icon="fas fa-sync" %}}Multi-Cloud Replication{{% /badge %}}
{{% badge style="green" icon="fas fa-coins" %}}Free-Tier Stacking{{% /badge %}}
{{% badge style="warning" title=" " icon="fas fa-chart-bar" %}}Built-in Dashboard{{% /badge %}}
{{% badge style="primary" icon="fas fa-tachometer-alt" %}}Quota Enforcement{{% /badge %}}
{{% badge style="danger" icon="fas fa-fire" %}}Prometheus Metrics{{% /badge %}}
{{% badge style="info" title=" " icon="fas fa-project-diagram" %}}Tempo Tracing{{% /badge %}}
{{% badge style="green" title=" " icon="fas fa-cubes" %}}Nomad HCL{{% /badge %}}
{{% badge style="warning" title=" " icon="fas fa-dharmachakra" %}}Kubernetes Manifests{{% /badge %}}
{{% badge style="info" icon="fas fa-bolt" %}}Object Data Cache{{% /badge %}}

</div>

<div style="text-align: center; margin-top: 1rem;">

{{% button href="docs/quickstart/" style="primary" icon="fas fa-rocket" %}}Quickstart{{% /button %}}
{{% button href="docs/" style="primary" icon="fas fa-book" %}}Documentation{{% /button %}}
{{% button href="godoc/" style="primary" icon="fas fa-code" %}}Go API{{% /button %}}
{{% button href="https://github.com/afreidah/s3-orchestrator" style="primary" icon="fab fa-github" %}}GitHub{{% /button %}}

</div>

<hr style="margin-top: 3rem;">

<h2 style="text-align: center; color: #2a9d73;">Unified S3 storage across multiple backends</h2>

<div class="hero-bullets">

- **Combine free-tier storage from multiple providers** into a single, larger pool - no cloud payment plans needed!

</div>

<div style="max-width: 700px; margin: 0 auto;">
{{< mermaid >}}
flowchart LR
    C([S3 Clients]):::client --> O[s3-orchestrator<br/>quota routing]:::orch
    O -->|"12/20 GB used"| B1[OCI Object Storage<br/>quota: 20 GB]:::backend
    O -->|"10/10 GB full"| B2[Backblaze B2<br/>quota: 10 GB]:::full
    O -->|"3/5 GB used"| B3[AWS S3<br/>quota: 5 GB]:::backend
    B1 & B2 & B3 -.- POOL([35 GB unified · 25 GB used · 10 GB free]):::pool

    classDef client fill:#6b4c2a,stroke:#d4a05a,color:#fff,font-weight:bold
    classDef orch fill:#7a5a30,stroke:#e8c070,color:#fff,font-weight:bold
    classDef backend fill:#3a2e20,stroke:#c4a35a,color:#e8dfd0
    classDef full fill:#3a2e20,stroke:#8b3a3a,color:#d4a0a0
    classDef pool fill:none,stroke:#d4a05a,color:#d4a05a,stroke-dasharray:5 5
{{< /mermaid >}}
</div>

<div class="hero-bullets">

- **Transparent multi-cloud replication** keeps copies across providers with automatic failover on read

</div>

<div style="max-width: 600px; margin: 0 auto;">
{{< mermaid >}}
flowchart LR
    C([S3 Client]):::client --> O[s3-orchestrator<br/>replication factor: 2]:::orch
    O -->|write| B1[Backend A]:::backend
    O -.->|replicate| B2[Backend B]:::backend
    B1 -->|read fails| O
    O -->|failover read| B2

    classDef client fill:#6b4c2a,stroke:#d4a05a,color:#fff,font-weight:bold
    classDef orch fill:#7a5a30,stroke:#e8c070,color:#fff,font-weight:bold
    classDef backend fill:#3a2e20,stroke:#c4a35a,color:#e8dfd0
{{< /mermaid >}}
</div>

<div class="hero-bullets">

- **Drop-in S3 replacement** - any tool that speaks S3 (aws cli, rclone, SDKs) works with zero code changes

</div>

<div style="max-width: 600px; margin: 0 auto;">
{{< mermaid >}}
flowchart TD
    CLI[aws cli]:::tool --> O[s3-orchestrator<br/>:9000]:::orch
    RC[rclone]:::tool --> O
    SDK[Python / Go / JS<br/>S3 SDKs]:::tool --> O
    TF[Terraform<br/>S3 backend]:::tool --> O

    classDef tool fill:#3a2e20,stroke:#c4a35a,color:#e8dfd0
    classDef orch fill:#7a5a30,stroke:#e8c070,color:#fff,font-weight:bold
{{< /mermaid >}}
</div>

<hr style="margin-top: 3rem;">

<h2 style="text-align: center; color: #2a9d73;">Key Features</h2>

<div class="feature-grid">
  <div class="feature-item">
    <i class="fas fa-layer-group feature-icon" style="color: #2a9d73;"></i>
    <div>
      <strong>Multi-Backend Storage</strong>
      <p>Stack allocations from different providers into a single, larger storage target.</p>
    </div>
    <div class="feature-detail">Combine free-tier allocations from OCI Object Storage, Backblaze B2, AWS S3, MinIO, Wasabi, or any S3-compatible provider. The orchestrator routes writes based on available quota and presents all backends as one unified endpoint.</div>
  </div>
  <div class="feature-item">
    <i class="fas fa-tachometer-alt feature-icon" style="color: #7dd3fc;"></i>
    <div>
      <strong>Per-Backend Quotas</strong>
      <p>Cap each backend at the exact byte limit to avoid surprise bills.</p>
    </div>
    <div class="feature-detail">Set a byte limit on each backend and the orchestrator enforces it atomically on every write. When a backend fills up, writes overflow to the next available backend automatically. Set quota to 0 to disable enforcement.</div>
  </div>
  <div class="feature-item">
    <i class="fas fa-sync feature-icon" style="color: #fca5a5;"></i>
    <div>
      <strong>Cross-Backend Replication</strong>
      <p>Automatic multi-cloud redundancy with zero client-side changes.</p>
    </div>
    <div class="feature-detail">Set a replication factor and a background worker ensures every object exists on that many backends. Objects are written to one backend on PUT; the replicator asynchronously copies them to reach the target factor.</div>
  </div>
  <div class="feature-item">
    <i class="fas fa-boxes feature-icon" style="color: #6ee7b7;"></i>
    <div>
      <strong>Virtual Buckets</strong>
      <p>Isolated namespaces and independent credentials per application.</p>
    </div>
    <div class="feature-detail">Each bucket has its own SigV4 access key and secret key. Objects are stored with an internal key prefix so bucket isolation requires zero changes to the storage layer or database schema.</div>
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
    <i class="fas fa-plug feature-icon" style="color: #2a9d73;"></i>
    <div>
      <strong>S3-Compatible API</strong>
      <p>Works with aws cli, rclone, any standard S3 client or SDK.</p>
    </div>
    <div class="feature-detail">Supports PutObject, GetObject, HeadObject, DeleteObject, CopyObject, ListObjectsV2, multipart uploads, range reads, and user metadata. Any tool that speaks S3 works with no modifications.</div>
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
    <i class="fas fa-traffic-light feature-icon" style="color: #fca5a5;"></i>
    <div>
      <strong>Usage Limits</strong>
      <p>Cap monthly API requests, egress, and ingress per backend.</p>
    </div>
    <div class="feature-detail">Set monthly caps on API requests, egress bytes, and ingress bytes per backend. When a backend exceeds a limit, writes overflow to other backends and reads fail over to replicas. Adaptive flushing shortens the tracking interval as limits approach.</div>
  </div>
  <div class="feature-item">
    <i class="fas fa-clock feature-icon" style="color: #6ee7b7;"></i>
    <div>
      <strong>Lifecycle Management</strong>
      <p>Automatic object expiration with configurable rules.</p>
    </div>
    <div class="feature-detail">Define expiration rules that target specific key prefixes - for example, automatically clean up temporary uploads or cache objects after a set period. Only objects matching the configured prefix patterns are expired; everything else in the bucket is left untouched. A background worker handles deletion of both backend storage and database metadata.</div>
  </div>
  <div class="feature-item">
    <i class="fas fa-binoculars feature-icon" style="color: #c4b5fd;"></i>
    <div>
      <strong>Observability</strong>
      <p>Prometheus metrics, OpenTelemetry tracing, structured audit logging.</p>
    </div>
    <div class="feature-detail">Exposes Prometheus metrics for all operations, quotas, and background tasks. Ships with a pre-built Grafana dashboard covering request rates, latency, backend health, quota usage, and replication status. OpenTelemetry tracing with configurable sampling. Structured JSON audit logs with request ID correlation across HTTP and storage layers.</div>
  </div>
  <div class="feature-item">
    <i class="fas fa-bolt feature-icon" style="color: #7dd3fc;"></i>
    <div>
      <strong>Object Data Cache</strong>
      <p>In-memory LRU cache for read-heavy workloads.</p>
    </div>
    <div class="feature-detail">Optional in-memory LRU cache that serves repeated reads from local storage, reducing backend API calls and egress. Configurable maximum cache size, per-object size limit, and TTL. Automatically invalidated on writes and deletes to ensure consistency. Ideal for read-heavy workloads where the same objects are fetched frequently.</div>
  </div>
</div>

<hr style="margin-top: 3rem;">

<h2 style="text-align: center; color: #2a9d73;">Who Is This For?</h2>

<div class="feature-grid">
  <div class="feature-item">
    <i class="fas fa-user-cog feature-icon" style="color: #2a9d73;"></i>
    <div>
      <strong>Homelabbers</strong>
      <p>Stack free-tier allocations from multiple providers into usable storage without paying for a single plan.</p>
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
    <i class="fas fa-building feature-icon" style="color: #e2b07a;"></i>
    <div>
      <strong>Small Teams and Startups</strong>
      <p>Get multi-cloud redundancy and encryption without the cost or complexity of enterprise storage platforms.</p>
    </div>
  </div>
  <div class="feature-item">
    <i class="fas fa-shield-alt feature-icon" style="color: #c4b5fd;"></i>
    <div>
      <strong>Anyone Who Wants Provider Independence</strong>
      <p>Avoid vendor lock-in. Your applications talk S3 to one endpoint - swap, add, or remove backends without touching a line of code.</p>
    </div>
  </div>
</div>

<hr style="margin-top: 3rem;">

<h2 style="text-align: center; color: #2a9d73;">Admin Web Interface</h2>

A built-in web dashboard provides real-time storage summaries, per-backend quota and usage bars, monthly traffic charts, a lazy-loaded directory tree for browsing and managing objects, and admin controls for rebalancing, syncing, uploading, and deleting files and folders.

![Admin Web Interface](/docs/images/free-tier-5-cloud-setup.png)

<hr style="margin-top: 3rem;">

<h2 style="text-align: center; color: #2a9d73;">Built-in Monitoring</h2>

s3-orchestrator ships with a pre-built Grafana dashboard and Prometheus metrics out of the box. Track request rates, latency percentiles, backend health, quota usage, replication progress, and background task performance - all without writing a single query.

![Grafana Dashboard](images/grafana.png)
