---
title: "Guides"
seoTitle: "s3-orchestrator guides"
description: "Step-by-step walkthroughs: stacking provider free tiers, multi-cloud redundancy, MinIO replication, and deployment on Nomad or systemd."
weight: 30
cascade:
  type: docs
---

Step-by-step walkthroughs for common deployments and operations.

{{< cards >}}
  {{< card link="local-demo/" title="Nomad/k8s Full Stack Demo" icon="play" subtitle="A complete environment on Nomad or Kubernetes with three MinIO backends and full observability." >}}
  {{< card link="maximizing-free-tiers/" title="Maximizing Free Tiers" icon="cash" subtitle="Combine free-tier storage from several providers into one pool without exceeding any provider's limits." >}}
  {{< card link="access-control/" title="Setting Up Access Control" icon="shield-check" subtitle="Declare the administering credential, onboard a client with only the access it needs, and scope an operator to the control plane." >}}
  {{< card link="terraform-provider/" title="Provisioning with Terraform" icon="cube" subtitle="Declare the buckets, users, keypairs and grants a deployment serves." >}}
  {{< card link="replication-guide/" title="Understanding Replication" icon="duplicate" subtitle="How the replicator works, target selection, side effects, over-replication cleanup and monitoring." >}}
  {{< card link="local-cloud-replication/" title="Local to Cloud Replication" icon="cloud-upload" subtitle="Keep a copy of a self-hosted S3 store in the cloud with no sync scripts or extra tooling." >}}
  {{< card link="multi-cloud-redundancy/" title="Simple Multi-Cloud Redundancy" icon="globe" subtitle="Multi-cloud replication with automatic read failover and no application changes." >}}
  {{< card link="systemd-deployment/" title="Deploying with systemd" icon="terminal" subtitle="Install the Debian package and run as a hardened systemd service with hot reload." >}}
  {{< card link="nomad-vault-deployment/" title="Deploying on Nomad with Vault" icon="server" subtitle="Run as a Nomad job with Vault-managed secrets and Transit encryption." >}}
  {{< card link="event-notifications/" title="Event Notifications" icon="bell" subtitle="Webhook notifications when objects are created or deleted, or backends change state." >}}
  {{< card link="encrypting-existing-data/" title="Encrypting Existing Data" icon="lock-closed" subtitle="Enable server-side encryption and encrypt the objects already stored." >}}
  {{< card link="key-rotation/" title="Key Rotation" icon="key" subtitle="Rotate encryption master keys with no downtime and no data re-encryption." >}}
  {{< card link="tui-object-browser/" title="Browsing Objects with the TUI" icon="view-list" subtitle="Explore the object namespace and inspect where every replica lives from the terminal." >}}
{{< /cards >}}
