---
title: "Local to Cloud Replication"
description: "Keep a copy of a self-hosted S3 store in the cloud automatically, with no sync scripts or additional tooling required."
weight: 4
aliases:
  - /guides/minio-cloud-replication/
---


This guide shows how to keep every object in a self-hosted S3-compatible store - SeaweedFS, Garage, RustFS, Ceph RGW, MinIO or any other - copied to a cloud backend using S3 Orchestrator, with no additional tooling or sync scripts required.

## Overview

Local storage carries risk - hardware failures, power loss, or site disasters can cause data loss. By placing S3 Orchestrator in front of your local store and adding a single cloud backend with a replication factor of 2, every object you write lands on both the local store and the cloud.

That is all it takes: one cloud backend, `replication.factor: 2`, and parallel copies on the write path. Each write uploads to both backends at once, and the built-in replicator fills any copy a write could not place. No cron jobs, rsync scripts, or third-party sync tools are needed.

If the local store goes down, reads automatically fail over to the cloud copy.

## Prerequisites

- A running self-hosted store reachable over the S3 API
- One cloud S3-compatible storage account (OCI, B2, R2, AWS, Wasabi, etc.)
- S3 Orchestrator installed and running
- Familiarity with the [configuration format](../../docs/user-guide/)

## Step 1: Configure the Local Store as the Primary Backend

Add your local store as the first backend. The `pack` routing strategy ensures it receives every write first.

```yaml
routing_strategy: pack

backends:
  - name: local
    endpoint: http://storage.local:9000
    region: us-east-1
    bucket: data
    access_key_id: "${LOCAL_ACCESS_KEY}"
    secret_access_key: "${LOCAL_SECRET_KEY}"
    force_path_style: true  # most self-hosted stores address buckets by path, not subdomain
    quota_bytes: 107374182400  # 100 GB - set to the space the store has
```

Either every backend sets `quota_bytes` or none does; a config that mixes the two is refused at startup. Since the cloud backend below has a quota, the local store needs one too.

## Step 2: Add a Cloud Backend

Add a cloud backend as the second copy's home. This example uses Cloudflare R2, but any S3-compatible provider works. See the [Maximizing Free Tiers](../maximizing-free-tiers/) guide for where to find credentials on each provider.

```yaml
backends:
  - name: local
    endpoint: http://storage.local:9000
    region: us-east-1
    bucket: data
    access_key_id: "${LOCAL_ACCESS_KEY}"
    secret_access_key: "${LOCAL_SECRET_KEY}"
    force_path_style: true
    quota_bytes: 107374182400

  - name: r2
    endpoint: https://<account-id>.r2.cloudflarestorage.com
    region: auto
    bucket: local-backup
    access_key_id: "${R2_ACCESS_KEY}"
    secret_access_key: "${R2_SECRET_KEY}"
    quota_bytes: 10737418240  # 10 GB
```

## Step 3: Enable Replication

Set the replication factor to 2 and turn on parallel copies. The factor tells the orchestrator that every object must exist on **two** backends - the local store and the cloud backend. Parallel copies has the write place both of them itself.

```yaml
replication:
  factor: 2

write_path:
  parallel_copies:
    enabled: true
```

That is the entire replication setup. Each upload goes to the local store and R2 at the same time, and the client is answered as soon as the first copy is committed; the second finishes on its own. No manual sync step, no scheduled jobs.

The background replicator still runs, as the catch-all for copies a write did not place: an R2 upload that failed or was refused, writes made while one backend was down, objects imported with `sync`, and writes that found the `max_in_flight` ceiling full. Without parallel copies the replicator makes every second copy, which means reading each object back off the local store before uploading it to R2.

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

Credentials declared in the config file are the simplest setup. Users, their credentials, and grants that give each user only the permissions it needs on a bucket can also be managed at runtime with the `s3-orchestrator admin` CLI, without a restart; see [Authentication](../../docs/authentication/) and the [Setting Up Access Control](../access-control/) guide.

## Step 5: Point Clients at the Orchestrator

Update your application or S3 client to use the orchestrator's endpoint instead of the local store directly.

```bash
aws configure set default.endpoint_url http://orchestrator-host:9000
aws configure set aws_access_key_id $CLIENT_ACCESS_KEY
aws configure set aws_secret_access_key $CLIENT_SECRET_KEY

# The write uploads to the local store and R2 at once
aws s3 cp myfile.txt s3://myapp/myfile.txt
```

## How It Works

With parallel copies on, a write claims the top two eligible backends - with `pack` routing, the local store first and R2 second - and uploads to both from one copy of the body. The client is answered once the first copy commits.

```
                                   +-->  local store  (copy 1)
Client writes  --->  Orchestrator -+
                                   +-->  R2           (copy 2)

                     Replicator  ------>  fills any copy a write missed
```

A write fails only when no copy lands. If R2 refuses or fails its upload, the write still succeeds on the local store, and the replicator places the R2 copy on its next pass.

If the local store is unavailable - network outage, hardware failure, power loss - the orchestrator serves reads from the R2 copy transparently. Writes also fall through to R2 so your application keeps working.

When the local store comes back online, the replicator finds that objects written during the outage exist on only one backend and copies them back, so both backends end up with a complete set of objects without any manual intervention.

No changes are needed on the local store. It does not need to know about the orchestrator or the cloud backend.

{{< callout type="info" >}}
**Encryption, compression and replication:** Objects are compressed and then encrypted before they are written to any backend, if those features are enabled. The replicator copies the stored bytes as they are - it never decodes or decrypts them - so every copy carries the same stored form and any copy can be read back by the orchestrator.
{{< /callout >}}

## Monitoring Replication

Track replication progress through the web dashboard or Prometheus metrics:

- **Replication backlog**: `s3o_replication_pending` - objects still short of their factor
- **Replicator copies**: `s3o_replication_copies_created_total` - copies the replicator made
- **Copies still uploading**: `s3o_detached_uploads_depth` - second copies still in flight after the client was answered; a rising floor means R2 is falling behind
- **Writes that fell back**: `s3o_replication_write_fanout_skipped_total` - writes that found the `max_in_flight` ceiling full and left the second copy to the replicator
- **Per-backend storage**: `s3o_quota_bytes_used` - confirms objects are landing on R2

{{< callout type="warning" >}}
Every copy counts against the cloud backend's quota and usage limits. If you set a 10 GB quota on R2, size it to hold the full contents of your local store. Compression, below, shrinks what each copy occupies.
{{< /callout >}}

## Important Notes

- Parallel copies moves the second copy's upload to write time. If your uplink is the bottleneck, PUT throughput can fall even as the local store stops paying for read-backs; measure before and after.
- Without parallel copies, replication is asynchronous: there is a window after each write where the object exists only on the local store, until the replicator's next pass copies it.
- The replicator is idempotent - restarting the orchestrator does not duplicate objects
- To keep copies with several cloud providers, add more backends and increase the replication factor
- Objects already in the local store can be imported with `s3-orchestrator sync --backend local --bucket myapp`. Only objects under the virtual bucket's prefix in the backend bucket (`myapp/...`) are imported as part of `myapp` and replicated. Objects at the root of the bucket are imported as unmanaged: they count toward the backend's quota, but clients cannot read them through the orchestrator and they are never replicated. Copy them under `myapp/` before importing ([#1765](https://github.com/afreidah/s3-orchestrator/issues/1765) tracks importing them in place)

## Optional: Enable Compression

The cloud backend meters storage and, on most providers, egress. Compression stores objects as chunked zstd, so each copy occupies and transfers fewer bytes, and the compressed size is what counts against `quota_bytes` and the usage limits:

```yaml
compression:
  enabled: true
```

Clients still see the object they wrote: sizes, ETags and checksums are those of the original. Objects that do not compress well, such as media and archives, are stored as they are. New writes are compressed after a restart; to compress objects already stored, run:

```bash
s3-orchestrator admin compress-existing
```

See [Compression](../../docs/compression/) for the levels, thresholds and metrics.

## Optional: Enable Encryption

To encrypt all data before it leaves the orchestrator (so backends only ever store ciphertext), add an `encryption` section to your config. The key must be exactly 32 bytes once base64-decoded:

```yaml
encryption:
  enabled: true
  master_key: "${ENCRYPTION_KEY}"
```

Generate a key with `openssl rand -base64 32` and store it securely.

After restarting the orchestrator, all new writes are encrypted automatically. Both the copy on the local store and the copy on the cloud backend are encrypted - backends never see plaintext.

To encrypt objects that were uploaded before encryption was enabled, run the encryption pass with the admin CLI:

```bash
s3-orchestrator admin encrypt-existing
```

It reports how many objects it encrypted, skipped and failed when it finishes. Each copy is rewritten to a new path on its own backend, so it is encrypted on the local store and R2 alike. `-max N` stops after N objects, so a large bucket can be converted over several runs.

For production deployments, consider using [Vault Transit](../nomad-vault-deployment/) instead of an inline key. See the [Encrypting Existing Data](../encrypting-existing-data/) guide for the full walkthrough and the [Key Rotation](../key-rotation/) guide for rotating master keys.
