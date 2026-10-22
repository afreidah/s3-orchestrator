---
title: "Local to Cloud Replication"
description: "Replicate objects from a local MinIO instance to a cloud backend automatically, with no sync scripts or additional tooling required."
weight: 4
---


This guide shows how to automatically replicate objects from a local MinIO instance to a cloud backend using S3 Orchestrator, with no additional tooling or sync scripts required.

## Overview

MinIO is a popular self-hosted S3-compatible object store, but local storage carries risk - hardware failures, power loss, or site disasters can cause data loss. By placing S3 Orchestrator in front of your MinIO instance and adding a single cloud backend with a replication factor of 2, every object you write to MinIO is automatically copied to the cloud in the background.

That is all it takes: one cloud backend and `replication.factor: 2`. The orchestrator's built-in replicator handles the rest. No cron jobs, rsync scripts, or third-party sync tools are needed.

If the local MinIO goes down, reads automatically fail over to the cloud replica.

## Prerequisites

- A running MinIO instance accessible via S3 API
- One cloud S3-compatible storage account (OCI, B2, R2, AWS, Wasabi, etc.)
- S3 Orchestrator installed and running
- Familiarity with the [configuration format](../../docs/user-guide/)

## Step 1: Configure MinIO as the Primary Backend

Add your MinIO instance as the first backend. The `pack` routing strategy ensures MinIO receives all primary writes first.

```yaml
routing_strategy: pack

backends:
  - name: local-minio
    endpoint: http://minio.local:9000
    region: us-east-1
    bucket: data
    access_key_id: "${MINIO_ACCESS_KEY}"
    secret_access_key: "${MINIO_SECRET_KEY}"
    force_path_style: true  # MinIO addresses buckets by path, not subdomain
    quota_bytes: 107374182400  # 100 GB - set to the space MinIO has
```

Either every backend sets `quota_bytes` or none does; a config that mixes the two is refused at startup. Since the cloud backend below has a quota, MinIO needs one too.

## Step 2: Add a Cloud Backend

Add a cloud backend as the replication target. This example uses Cloudflare R2, but any S3-compatible provider works. See the [Maximizing Free Tiers](../maximizing-free-tiers/) guide for where to find credentials on each provider.

```yaml
backends:
  - name: local-minio
    endpoint: http://minio.local:9000
    region: us-east-1
    bucket: data
    access_key_id: "${MINIO_ACCESS_KEY}"
    secret_access_key: "${MINIO_SECRET_KEY}"
    force_path_style: true
    quota_bytes: 107374182400

  - name: r2
    endpoint: https://<account-id>.r2.cloudflarestorage.com
    region: auto
    bucket: minio-backup
    access_key_id: "${R2_ACCESS_KEY}"
    secret_access_key: "${R2_SECRET_KEY}"
    quota_bytes: 10737418240  # 10 GB
```

## Step 3: Enable Replication

Set the replication factor to 2. This tells the orchestrator that every object must exist on **two** backends - your local MinIO and the cloud backend.

```yaml
replication:
  factor: 2
```

That is the entire replication setup. The orchestrator writes objects to MinIO on upload, and the background replicator automatically copies each one to R2. No manual sync step, no scheduled jobs.

## Step 4: Configure a Virtual Bucket

Create a virtual bucket with credentials for your applications.

```yaml
buckets:
  - name: myapp
    credentials:
      - access_key_id: "${CLIENT_ACCESS_KEY}"
        secret_access_key: "${CLIENT_SECRET_KEY}"
```

Generate a credential pair if you do not already have one:

```bash
echo "Access Key: $(openssl rand -hex 10 | tr '[:lower:]' '[:upper:]')"
echo "Secret Key: $(openssl rand -base64 30)"
```

The bucket's credential reaches only the bucket. The `s3-orchestrator admin` commands used below need the administering credential, declared under `auth.root` with a pair generated the same way:

```yaml
auth:
  root:
    access_key_id: "${ADMIN_ACCESS_KEY}"
    secret_access_key: "${ADMIN_SECRET_KEY}"
```

The admin CLI signs its requests with that pair, read from `S3O_ACCESS_KEY_ID` and `S3O_SECRET_ACCESS_KEY`, and finds the instance through `S3O_ADMIN_ADDR`:

```bash
export S3O_ADMIN_ADDR=http://orchestrator-host:9000
export S3O_ACCESS_KEY_ID=$ADMIN_ACCESS_KEY
export S3O_SECRET_ACCESS_KEY=$ADMIN_SECRET_KEY
```

## Step 5: Point Clients at the Orchestrator

Update your application or S3 client to use the orchestrator's endpoint instead of MinIO directly.

```bash
aws configure set default.endpoint_url http://orchestrator-host:9000
aws configure set aws_access_key_id $CLIENT_ACCESS_KEY
aws configure set aws_secret_access_key $CLIENT_SECRET_KEY

# Writes go to local MinIO, replicator copies to cloud
aws s3 cp myfile.txt s3://myapp/myfile.txt
```

## How It Works

With `pack` routing, all writes go to local MinIO first since it is listed before the cloud backend. The replicator sees that each new object only exists on one backend (MinIO) and needs to be on two (the replication factor), so it copies the object to R2 in the background.

```
Client writes  --->  Orchestrator  --->  MinIO (primary write)
                                           |
                     Replicator  --------  +-->  R2 (async copy)
```

If MinIO is unavailable - network outage, hardware failure, power loss - the orchestrator serves reads from the R2 replica transparently. Writes also fall through to R2 so your application keeps working.

When MinIO comes back online, the replication worker detects that objects written to R2 during the outage only exist on one backend. It automatically copies them back to MinIO to restore the replication factor, so both backends end up with a complete set of objects without any manual intervention.

No changes are needed on the MinIO side. It does not need to know about the orchestrator or the cloud backend.

{{< callout type="info" >}}
**Encryption and replication:** If encryption is enabled, objects are encrypted before they are written to any backend. Replicas are copied as opaque ciphertext — the replicator does not decrypt and re-encrypt, it streams the encrypted bytes directly. All copies share the same encryption key metadata, so any copy can be decrypted by the orchestrator. Enabling or disabling encryption does not affect the replication process.
{{< /callout >}}

## Monitoring Replication

Track replication progress through the web dashboard or Prometheus metrics:

- **Replication queue depth**: `s3o_replication_pending` - objects waiting to be replicated
- **Replication operations**: `s3o_replication_copies_created_total` - completed copies
- **Per-backend storage**: `s3o_quota_bytes_used` - confirms objects are landing on R2

{{< callout type="warning" >}}
Replication copies count against the cloud backend's quota and usage limits. If you set a 10 GB quota on R2, that 10 GB is shared between direct writes and replicated objects. Size your quota to accommodate the full contents of your MinIO instance.
{{< /callout >}}

## Important Notes

- Replication is asynchronous by default - there is a brief window after a write where the object exists only on MinIO before the copy completes. Setting `write_path.parallel_copies.enabled: true` has the write upload to both backends at once instead, narrowing that window to the length of one upload and sparing MinIO the read-back the replicator would otherwise make
- The replicator is idempotent - restarting the orchestrator does not duplicate objects
- To replicate to multiple cloud providers for extra safety, add more backends and increase the replication factor
- Objects already in MinIO can be imported with `s3-orchestrator sync --backend local-minio --bucket myapp`. Only objects under the virtual bucket's prefix in the MinIO bucket (`myapp/...`) are imported as part of `myapp` and replicated. Objects at the root of the bucket are imported as unmanaged: they count toward the backend's quota, but clients cannot read them through the orchestrator and they are never replicated. Copy them under `myapp/` in MinIO before importing ([#1765](https://github.com/afreidah/s3-orchestrator/issues/1765) tracks importing them in place)

## Optional: Enable Encryption

To encrypt all data before it leaves the orchestrator (so backends only ever store ciphertext), add an `encryption` section to your config:

```yaml
encryption:
  enabled: true
  master_key: "${ENCRYPTION_KEY}"
```

Generate a key with `openssl rand -base64 32` and store it securely.

After restarting the orchestrator, all new writes are encrypted automatically. Both the primary copy on MinIO and the replica on the cloud backend are encrypted — backends never see plaintext.

To encrypt objects that were uploaded before encryption was enabled, run the encryption pass with the admin CLI:

```bash
s3-orchestrator admin encrypt-existing
```

It reports how many objects it encrypted, skipped and failed when it finishes. Each copy is rewritten to a new path on its own backend, so it is encrypted on MinIO and R2 alike. `-max N` stops after N objects, so a large bucket can be converted over several runs.

For production deployments, consider using [Vault Transit](../nomad-vault-deployment/) instead of an inline key. See the [Encrypting Existing Data](../encrypting-existing-data/) guide for the full walkthrough and the [Key Rotation](../key-rotation/) guide for rotating master keys.
