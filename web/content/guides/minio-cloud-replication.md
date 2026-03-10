---
title: "Local to Cloud Replication"
weight: 4
---


This guide shows how to automatically replicate objects from a local MinIO instance to a cloud backend using S3 Orchestrator, with no additional tooling or sync scripts required.

## Overview

MinIO is a popular self-hosted S3-compatible object store, but local storage carries risk - hardware failures, power loss, or site disasters can cause data loss. By placing S3 Orchestrator in front of your MinIO instance and adding a single cloud backend with a replication factor of 2, every object you write to MinIO is automatically copied to the cloud in the background.

That is all it takes: one cloud backend and `replication_factor: 2`. The orchestrator's built-in replicator handles the rest. No cron jobs, rsync scripts, or third-party sync tools are needed.

If the local MinIO goes down, reads automatically fail over to the cloud replica.

## Prerequisites

- A running MinIO instance accessible via S3 API
- One cloud S3-compatible storage account (OCI, B2, R2, AWS, Wasabi, etc.)
- S3 Orchestrator installed and running
- Familiarity with the [configuration format](../../docs/user-guide.md)

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
    quota: 0  # No quota - local storage is yours
```

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
    quota: 0

  - name: r2
    endpoint: https://<account-id>.r2.cloudflarestorage.com
    region: auto
    bucket: minio-backup
    access_key_id: "${R2_ACCESS_KEY}"
    secret_access_key: "${R2_SECRET_KEY}"
    quota: 10737418240  # 10 GB
```

## Step 3: Enable Replication

Set the replication factor to 2. This tells the orchestrator that every object must exist on **two** backends - your local MinIO and the cloud backend.

```yaml
replication_factor: 2
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

## Monitoring Replication

Track replication progress through the web dashboard or Prometheus metrics:

- **Replication queue depth**: `s3proxy_replication_queue_depth` - objects waiting to be replicated
- **Replication operations**: `s3proxy_replication_operations_total` - completed copies
- **Per-backend storage**: `s3proxy_backend_used_bytes` - confirms objects are landing on R2

{{% notice warning %}}
Replication copies count against the cloud backend's quota and usage limits. If you set a 10 GB quota on R2, that 10 GB is shared between direct writes and replicated objects. Size your quota to accommodate the full contents of your MinIO instance.
{{% /notice %}}

## Important Notes

- Replication is asynchronous - there is a brief window after a write where the object exists only on MinIO before the copy completes
- The replicator is idempotent - restarting the orchestrator does not duplicate objects
- To replicate to multiple cloud providers for extra safety, add more backends and increase the replication factor
- If you have existing objects in MinIO, the orchestrator can discover and replicate them via the admin sync operation
