---
title: "s3-orchestrator go api reference"
linkTitle: "Go API Reference"
chapter: true
weight: 40
---


<div class="landing-subheader">Generated documentation for the Go packages in this project.</div>

### Core

<div class="landing-grid">
  <a class="landing-card" href="proxy/">
    <i class="fas fa-database landing-card-icon" style="color: #93c5fd;"></i>
    <div>
      <strong>proxy</strong>
      <p>Multi-backend S3 object orchestration with quota enforcement, replication, and rebalancing.</p>
    </div>
  </a>
  <a class="landing-card" href="store/">
    <i class="fas fa-hdd landing-card-icon" style="color: #a5b4fc;"></i>
    <div>
      <strong>store</strong>
      <p>PostgreSQL metadata store with circuit breaker wrapper and role-based interfaces.</p>
    </div>
  </a>
  <a class="landing-card" href="backend/">
    <i class="fas fa-cloud landing-card-icon" style="color: #67e8f9;"></i>
    <div>
      <strong>backend</strong>
      <p>S3-compatible storage client abstraction with circuit breaker wrapper.</p>
    </div>
  </a>
  <a class="landing-card" href="config/">
    <i class="fas fa-cog landing-card-icon" style="color: #6ee7b7;"></i>
    <div>
      <strong>config</strong>
      <p>YAML configuration loading, defaults, and validation.</p>
    </div>
  </a>
  <a class="landing-card" href="counter/">
    <i class="fas fa-tachometer-alt landing-card-icon" style="color: #fcd34d;"></i>
    <div>
      <strong>counter</strong>
      <p>Per-backend usage tracking with local atomic and Redis backends.</p>
    </div>
  </a>
  <a class="landing-card" href="encryption/">
    <i class="fas fa-lock landing-card-icon" style="color: #c4b5fd;"></i>
    <div>
      <strong>encryption</strong>
      <p>Envelope encryption with AES-256-GCM and pluggable key providers.</p>
    </div>
  </a>
  <a class="landing-card" href="cache/">
    <i class="fas fa-memory landing-card-icon" style="color: #a78bfa;"></i>
    <div>
      <strong>cache</strong>
      <p>In-memory LRU object data cache with size-aware eviction.</p>
    </div>
  </a>
  <a class="landing-card" href="breaker/">
    <i class="fas fa-bolt landing-card-icon" style="color: #f87171;"></i>
    <div>
      <strong>breaker</strong>
      <p>Generic three-state circuit breaker state machine.</p>
    </div>
  </a>
</div>

### Transport

<div class="landing-grid">
  <a class="landing-card" href="s3api/">
    <i class="fas fa-server landing-card-icon" style="color: #7dd3fc;"></i>
    <div>
      <strong>s3api</strong>
      <p>S3-compatible HTTP API with SigV4 authentication, rate limiting, and admission control.</p>
    </div>
  </a>
  <a class="landing-card" href="admin/">
    <i class="fas fa-tools landing-card-icon" style="color: #fca5a5;"></i>
    <div>
      <strong>admin</strong>
      <p>Admin API handler for operational control endpoints.</p>
    </div>
  </a>
  <a class="landing-card" href="ui/">
    <i class="fas fa-desktop landing-card-icon" style="color: #c4b5fd;"></i>
    <div>
      <strong>ui</strong>
      <p>Built-in web dashboard for storage management.</p>
    </div>
  </a>
  <a class="landing-card" href="auth/">
    <i class="fas fa-key landing-card-icon" style="color: #e2b07a;"></i>
    <div>
      <strong>auth</strong>
      <p>SigV4 request signing, presigned URLs, and credential validation.</p>
    </div>
  </a>
  <a class="landing-card" href="httputil/">
    <i class="fas fa-shield-alt landing-card-icon" style="color: #f9a8d4;"></i>
    <div>
      <strong>httputil</strong>
      <p>TLS certificate hot-reload, client IP extraction, and login throttle.</p>
    </div>
  </a>
</div>

### Background Services

<div class="landing-grid">
  <a class="landing-card" href="worker/">
    <i class="fas fa-cogs landing-card-icon" style="color: #86efac;"></i>
    <div>
      <strong>worker</strong>
      <p>Background workers for replication, rebalancing, cleanup, scrubbing, and reconciliation.</p>
    </div>
  </a>
  <a class="landing-card" href="notify/">
    <i class="fas fa-bell landing-card-icon" style="color: #fcd34d;"></i>
    <div>
      <strong>notify</strong>
      <p>Webhook notification delivery with durable outbox and retry.</p>
    </div>
  </a>
  <a class="landing-card" href="lifecycle/">
    <i class="fas fa-clock landing-card-icon" style="color: #6ee7b7;"></i>
    <div>
      <strong>lifecycle</strong>
      <p>Background service manager with panic recovery and graceful shutdown.</p>
    </div>
  </a>
</div>

### Observability

<div class="landing-grid">
  <a class="landing-card" href="telemetry/">
    <i class="fas fa-chart-line landing-card-icon" style="color: #7dd3fc;"></i>
    <div>
      <strong>telemetry</strong>
      <p>Prometheus metrics, OpenTelemetry tracing, and in-memory log buffer.</p>
    </div>
  </a>
  <a class="landing-card" href="audit/">
    <i class="fas fa-clipboard-list landing-card-icon" style="color: #93c5fd;"></i>
    <div>
      <strong>audit</strong>
      <p>Request ID propagation and structured audit logging.</p>
    </div>
  </a>
  <a class="landing-card" href="event/">
    <i class="fas fa-broadcast-tower landing-card-icon" style="color: #fbbf24;"></i>
    <div>
      <strong>event</strong>
      <p>Notification event type constants and emit hook.</p>
    </div>
  </a>
</div>

### Utilities

<div class="landing-grid">
  <a class="landing-card" href="bufpool/">
    <i class="fas fa-layer-group landing-card-icon" style="color: #d4d4d8;"></i>
    <div>
      <strong>bufpool</strong>
      <p>Shared sync.Pool byte buffers for streaming I/O.</p>
    </div>
  </a>
  <a class="landing-card" href="syncutil/">
    <i class="fas fa-sync landing-card-icon" style="color: #d4d4d8;"></i>
    <div>
      <strong>syncutil</strong>
      <p>AtomicConfig[T] and TTLCache[K,V] generic sync primitives.</p>
    </div>
  </a>
  <a class="landing-card" href="workerpool/">
    <i class="fas fa-tasks landing-card-icon" style="color: #d4d4d8;"></i>
    <div>
      <strong>workerpool</strong>
      <p>Generic bounded-concurrency Run[T] and Collect[T,R].</p>
    </div>
  </a>
</div>
