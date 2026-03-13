---
title: "s3-orchestrator guides"
linkTitle: "Guides"
chapter: true
weight: 30
---


<div class="landing-subheader">Step-by-step tutorials for common operations and deployment patterns.</div>

<div class="landing-grid">
  <a class="landing-card" href="local-demo/">
    <i class="fas fa-play-circle landing-card-icon" style="color: #86efac;"></i>
    <div>
      <strong>Nomad/k8s Full Stack Demo</strong>
      <p>Stand up a complete environment with Nomad or Kubernetes, three MinIO backends, and full observability in minutes.</p>
    </div>
  </a>
  <a class="landing-card" href="maximizing-free-tiers/">
    <i class="fas fa-coins landing-card-icon" style="color: #6ee7b7;"></i>
    <div>
      <strong>Maximizing Free Tiers</strong>
      <p>Combine free-tier storage from multiple cloud providers into a single pool without exceeding any provider's limits.</p>
    </div>
  </a>
  <a class="landing-card" href="replication-guide/">
    <i class="fas fa-copy landing-card-icon" style="color: #67e8f9;"></i>
    <div>
      <strong>Understanding Replication</strong>
      <p>How the replicator works, target selection, side-effects, over-replication cleanup, and monitoring.</p>
    </div>
  </a>
  <a class="landing-card" href="minio-cloud-replication/">
    <i class="fas fa-cloud-upload-alt landing-card-icon" style="color: #93c5fd;"></i>
    <div>
      <strong>Local to Cloud Replication</strong>
      <p>Automatically back up a local MinIO instance to the cloud with no sync scripts or additional tooling.</p>
    </div>
  </a>
  <a class="landing-card" href="multi-cloud-redundancy/">
    <i class="fas fa-globe landing-card-icon" style="color: #c4b5fd;"></i>
    <div>
      <strong>Simple Multi-Cloud Redundancy</strong>
      <p>Transparent multi-cloud replication with automatic failover - no application changes required.</p>
    </div>
  </a>
  <a class="landing-card" href="systemd-deployment/">
    <i class="fas fa-terminal landing-card-icon" style="color: #f9a8d4;"></i>
    <div>
      <strong>Deploying with systemd</strong>
      <p>Install the Debian package and run as a systemd service with security hardening and hot reload.</p>
    </div>
  </a>
  <a class="landing-card" href="nomad-vault-deployment/">
    <i class="fas fa-server landing-card-icon" style="color: #a78bfa;"></i>
    <div>
      <strong>Deploying on Nomad with Vault</strong>
      <p>Run the orchestrator as a Nomad job with Vault-managed secrets and Transit encryption.</p>
    </div>
  </a>
  <a class="landing-card" href="encrypting-existing-data/">
    <i class="fas fa-lock landing-card-icon" style="color: #fca5a5;"></i>
    <div>
      <strong>Encrypting Existing Data</strong>
      <p>Enable server-side encryption and migrate unencrypted objects already stored across your backends.</p>
    </div>
  </a>
  <a class="landing-card" href="key-rotation/">
    <i class="fas fa-key landing-card-icon" style="color: #e2b07a;"></i>
    <div>
      <strong>Key Rotation</strong>
      <p>Rotate encryption master keys with zero downtime and no data re-encryption.</p>
    </div>
  </a>
</div>
