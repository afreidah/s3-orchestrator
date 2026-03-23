---
title: "s3-orchestrator diagrams"
linkTitle: "Diagrams"
chapter: true
weight: 35
---


<div class="landing-subheader">Interactive architecture and flow diagrams for the S3 Orchestrator internals.</div>

<div class="landing-grid">
  <a class="landing-card" href="architecture/">
    <i class="fas fa-project-diagram landing-card-icon" style="color: #d29922;"></i>
    <div>
      <strong>System Architecture</strong>
      <p>End-to-end architecture showing the request path, storage layer, background services, and observability.</p>
    </div>
  </a>
  <a class="landing-card" href="admission-control/">
    <i class="fas fa-filter landing-card-icon" style="color: #58a6ff;"></i>
    <div>
      <strong>Admission Control Flow</strong>
      <p>Detailed request lifecycle through admission control, authentication, routing, and circuit breakers.</p>
    </div>
  </a>
  <a class="landing-card" href="write-path/">
    <i class="fas fa-upload landing-card-icon" style="color: #3fb950;"></i>
    <div>
      <strong>Write Path</strong>
      <p>PutObject flow through backend selection, encryption, failover, and metadata recording.</p>
    </div>
  </a>
  <a class="landing-card" href="read-path/">
    <i class="fas fa-download landing-card-icon" style="color: #bc8cff;"></i>
    <div>
      <strong>Read Path</strong>
      <p>GetObject flow through location lookup, failover, broadcast reads, decryption, and streaming.</p>
    </div>
  </a>
  <a class="landing-card" href="circuit-breaker/">
    <i class="fas fa-bolt landing-card-icon" style="color: #f85149;"></i>
    <div>
      <strong>Circuit Breaker</strong>
      <p>Three-state FSM for backends and database: closed, open, half-open transitions and probe logic.</p>
    </div>
  </a>
  <a class="landing-card" href="encryption/">
    <i class="fas fa-lock landing-card-icon" style="color: #a7d5c1;"></i>
    <div>
      <strong>Encryption Flow</strong>
      <p>Envelope encryption pipeline: DEK generation, key wrapping, chunk-based AES-256-GCM, and range decryption.</p>
    </div>
  </a>
  <a class="landing-card" href="background-services/">
    <i class="fas fa-cogs landing-card-icon" style="color: #8b949e;"></i>
    <div>
      <strong>Background Services</strong>
      <p>Periodic workers: replicator, rebalancer, cleanup queue, lifecycle, multipart cleanup, and usage flusher.</p>
    </div>
  </a>
  <a class="landing-card" href="database-schema/">
    <i class="fas fa-database landing-card-icon" style="color: #4aaa8a;"></i>
    <div>
      <strong>Database Schema</strong>
      <p>Entity-relationship diagram of the PostgreSQL metadata store: tables, columns, indexes, and relationships.</p>
    </div>
  </a>
</div>
