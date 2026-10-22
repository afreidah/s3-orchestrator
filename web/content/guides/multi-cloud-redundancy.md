---
title: "Simple Multi-Cloud Redundancy"
description: "Store every object across multiple providers transparently, with no changes required to your applications or existing S3 clients."
weight: 5
---


This guide shows how to set up transparent multi-cloud redundancy so that every object is stored across multiple providers, with no changes required to your applications or S3 clients.

## Overview

Relying on a single cloud provider for object storage means a provider outage, account suspension, or policy change can make your data inaccessible. S3 Orchestrator solves this by replicating objects across multiple S3-compatible providers behind a single endpoint.

Your applications connect to the orchestrator using standard S3 credentials and API calls. They have no knowledge of how many backends exist, where objects are stored, or that replication is happening. The orchestrator handles all of it.

## Prerequisites

- Accounts on two or more S3-compatible providers
- S3 Orchestrator installed and running
- Familiarity with the [configuration format](../../docs/user-guide/)

## Step 1: Configure Multiple Backends

Add backends from different providers. Using providers in different regions or cloud platforms gives you the strongest redundancy.

```yaml
routing_strategy: spread

backends:
  - name: oci
    endpoint: https://objectstorage.us-ashburn-1.oraclecloud.com
    region: us-ashburn-1
    bucket: primary-bucket
    access_key_id: "<oci-access-key>"
    secret_access_key: "<oci-secret-key>"
    quota_bytes: 10737418240  # 10 GB

  - name: r2
    endpoint: https://<account-id>.r2.cloudflarestorage.com
    region: auto
    bucket: primary-bucket
    access_key_id: "<r2-access-key>"
    secret_access_key: "<r2-secret-key>"
    quota_bytes: 10737418240  # 10 GB

  - name: b2
    endpoint: https://s3.us-west-004.backblazeb2.com
    region: us-west-004
    bucket: primary-bucket
    access_key_id: "<b2-access-key>"
    secret_access_key: "<b2-secret-key>"
    quota_bytes: 10737418240  # 10 GB
```

The `spread` strategy distributes writes evenly across backends, keeping storage usage balanced.

## Step 2: Set the Replication Factor

The replication factor controls how many copies of each object exist across your backends. Set it to the number of independent copies you want, and turn on parallel copies so the write places them itself.

```yaml
replication:
  factor: 2

write_path:
  parallel_copies:
    enabled: true
```

With three backends and a replication factor of 2, each upload goes to two backends at once and answers the client on the first that commits; the second finishes on its own. A factor of 3 would place a copy on every backend.

The background replicator is the catch-all for copies a write did not place: an upload that failed, writes made while a backend was down, imported objects, and writes that found the `max_in_flight` ceiling full. Without parallel copies the replicator makes every extra copy, which in a multi-cloud setup means reading each object back out of one provider and writing it to another, paying egress on the read.

{{< callout >}}
A replication factor of 2 protects against a single provider outage. A factor of 3 protects against two simultaneous provider failures. Choose based on how critical your data is.
{{< /callout >}}

## Step 3: Configure a Virtual Bucket

Create a virtual bucket with credentials for your application.

```yaml
buckets:
  - name: myapp
    credentials:
      - access_key_id: "<client-access-key>"
        secret_access_key: "<client-secret-key>"
```

## Step 4: Connect Your Application

Point your application at the orchestrator endpoint using the virtual bucket credentials. No special client libraries, plugins, or configuration flags are needed.

```bash
# AWS CLI
aws configure set default.endpoint_url http://orchestrator-host:9000
aws s3 cp report.pdf s3://myapp/report.pdf

# rclone
rclone copy report.pdf s3orchestrator:myapp/

# Python (boto3)
import boto3
s3 = boto3.client('s3',
    endpoint_url='http://orchestrator-host:9000',
    aws_access_key_id='<client-access-key>',
    aws_secret_access_key='<client-secret-key>')
s3.upload_file('report.pdf', 'myapp', 'report.pdf')
```

Your application writes to a single endpoint. It does not know about OCI, R2, B2, or any other backend. The orchestrator places both copies and the replicator fills anything a write missed.

## How Failover Works

When a client reads an object, the orchestrator knows which backends hold a copy. If the primary backend is unreachable - provider outage, network issue, or usage limit reached - the orchestrator serves the read from a replica on another backend.

This happens transparently. The client gets the object with no errors, retries, or configuration changes.

```
Client  --->  Orchestrator  --->  OCI (down)
                              |-> R2 (serves replica) <<<
                              |-> B2 (also has replica)
```

If a backend stays down longer than `replication.unhealthy_threshold` (default: 10 minutes), the replication worker creates replacement copies on healthy backends so the configured replication factor is maintained even during sustained outages. This requires `backend_circuit_breaker.enabled: true`.

## What Your Application Does Not Need to Know

| Concern | Handled By |
|---------|-----------|
| Which provider stores each object | Orchestrator routing |
| How many copies exist | Replication factor |
| What to do when a provider is down | Automatic failover |
| When a backend's quota is full | Overflow to next backend |
| When usage limits are reached | Fail over reads, overflow writes |

Your application speaks standard S3. Everything else is the orchestrator's responsibility.

## Monitoring

Use the web dashboard to see replication status at a glance, or query Prometheus metrics:

- **Replica count per object**: verify objects meet the target factor
- **Backend health**: `s3o_circuit_breaker_state{name="<backend>"}` - 0=healthy, 1=down, 2=probing
- **Failover events**: check structured logs for reads served from non-primary backends
- **Health-aware copies**: `s3o_replication_health_copies_total` - non-zero means the replicator is creating replacement copies for down backends
- **Copies still uploading**: `s3o_detached_uploads_depth` - copies still in flight after the client was answered; a rising floor means a provider is falling behind
- **Writes that fell back**: `s3o_replication_write_fanout_skipped_total` - writes that found the `max_in_flight` ceiling full and left their extra copies to the replicator

{{< callout type="warning" >}}
The client is answered on the first copy, not the last, so there is a window the length of one upload where the object exists on only one backend. Without parallel copies that window lasts until the replicator's next pass. Parallel copies also moves the extra uploads to write time: if your uplink is the bottleneck, PUT throughput can fall even as egress for read-backs goes away.
{{< /callout >}}

## Important Notes

- Adding or removing backends does not require application changes - update the config and restart the orchestrator
- With `rebalance.enabled: true`, the rebalancer moves existing objects onto new backends on its next run; it is off by default
- Compression shrinks every copy, and the compressed size is what counts against each provider's quota and usage limits; replicas carry the stored bytes as they are - see [Compression](../../docs/compression/)
- Encryption can be layered on top of replication - see the [Encrypting Existing Data](../encrypting-existing-data/) guide
- Free-tier limits on each provider can be enforced with quotas and usage limits - see the [Maximizing Free Tiers](../maximizing-free-tiers/) guide
