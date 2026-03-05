---
title: "Maximizing Free Tiers"
weight: 3
---

<div style="text-align: center; margin-bottom: 1.5rem;">
  <img src="/images/logo.png" alt="s3-orchestrator" style="max-width: 200px; height: auto;">
</div>

This guide walks through combining free-tier object storage from multiple cloud providers into a single, larger storage pool using S3 Orchestrator, from creating provider accounts to connecting your first application.

## Overview

Most S3-compatible providers offer a free tier with a limited amount of storage and API requests. Individually these allocations are small, but S3 Orchestrator lets you stack them behind a single endpoint. The orchestrator handles routing writes to backends with available quota, overflowing to the next backend when one fills up.

The key tools for staying within free tiers are **per-backend quotas** and **usage limits**. Quotas cap stored bytes so you never exceed a provider's free storage allowance. Usage limits cap monthly API requests, egress, and ingress so you avoid overage charges on metered dimensions.

## Prerequisites

- S3 Orchestrator installed and running (see the [Quickstart](../../docs/quickstart/))
- A PostgreSQL database for the orchestrator's metadata
- Accounts on two or more S3-compatible providers with free-tier allocations

## Step 1: Identify Your Free-Tier Allowances

Check each provider's free-tier limits. Common examples:

| Provider | Free Storage | Free API Requests | Free Egress |
|----------|-------------|-------------------|-------------|
| Oracle Cloud (OCI) | 10 GB | 50,000/mo | 10 GB/mo |
| Backblaze B2 | 10 GB | 2,500/day | 1 GB/day |
| Cloudflare R2 | 10 GB | 1,000,000/mo | Unlimited |

With just these three providers you get 30 GB of combined storage behind a single S3 endpoint.

## Step 2: Get Credentials from Each Provider

Each provider gives you an **access key** and **secret key** for their S3-compatible API. These are the credentials the orchestrator uses to read and write objects on that provider.

### Oracle Cloud (OCI)

1. Log in to the OCI Console
2. Go to **Profile** (top right) -> **My Profile** -> **Customer Secret Keys**
3. Click **Generate Secret Key**, give it a name
4. Copy the **Secret Key** immediately (it is only shown once)
5. The **Access Key** appears in the list after creation
6. Your S3 endpoint is `https://<namespace>.compat.objectstorage.<region>.oraclecloud.com` (find your namespace under **Tenancy Details**)
7. Create a bucket in **Object Storage** -> **Buckets**

### Backblaze B2

1. Log in to the B2 Console
2. Go to **Application Keys** -> **Add a New Application Key**
3. Set the key type to **Read and Write**, scope it to a specific bucket or all buckets
4. Copy the **keyID** (this is your access key) and **applicationKey** (this is your secret key)
5. Your S3 endpoint is `https://s3.<region>.backblazeb2.com` (the region is shown on your bucket page)
6. Create a bucket under **Buckets** -> **Create a Bucket**

### Cloudflare R2

1. Log in to the Cloudflare Dashboard
2. Go to **R2 Object Storage** -> **Manage R2 API Tokens**
3. Click **Create API Token**, grant **Object Read & Write** permission
4. Copy the **Access Key ID** and **Secret Access Key**
5. Your S3 endpoint is `https://<account-id>.r2.cloudflarestorage.com` (the account ID is on the R2 overview page)
6. Create a bucket under **R2** -> **Create bucket**

{{% notice tip %}}
Never commit provider credentials to version control. Use environment variables in your config file with the `${VAR}` syntax, and inject them via systemd `EnvironmentFile`, container secrets, or a secrets manager.
{{% /notice %}}

## Step 3: Configure Backends with Quotas

Add each provider as a backend in your `config.yaml`. Set each backend's `quota` to match the provider's free storage limit in bytes. This prevents writes from exceeding the allowance.

```yaml
backends:
  - name: oci
    endpoint: https://<namespace>.compat.objectstorage.us-ashburn-1.oraclecloud.com
    region: us-ashburn-1
    bucket: my-bucket
    access_key_id: "${OCI_ACCESS_KEY}"
    secret_access_key: "${OCI_SECRET_KEY}"
    force_path_style: true
    quota: 10737418240  # 10 GB in bytes

  - name: b2
    endpoint: https://s3.us-west-004.backblazeb2.com
    region: us-west-004
    bucket: my-bucket
    access_key_id: "${B2_ACCESS_KEY}"
    secret_access_key: "${B2_SECRET_KEY}"
    quota: 10737418240  # 10 GB

  - name: r2
    endpoint: https://<account-id>.r2.cloudflarestorage.com
    region: auto
    bucket: my-bucket
    access_key_id: "${R2_ACCESS_KEY}"
    secret_access_key: "${R2_SECRET_KEY}"
    quota: 10737418240  # 10 GB
```

When OCI fills up, writes automatically overflow to B2, then to R2.

## Step 4: Set Usage Limits

Usage limits prevent you from exceeding free-tier API request and bandwidth allowances.

```yaml
backends:
  - name: oci
    # ... endpoint, credentials, quota as above
    usage_limits:
      max_api_requests: 50000
      max_egress_bytes: 10737418240   # 10 GB
      max_ingress_bytes: 10737418240  # 10 GB

  - name: b2
    # ... endpoint, credentials, quota as above
    usage_limits:
      max_api_requests: 75000         # ~2500/day * 30 days
      max_egress_bytes: 1073741824    # 1 GB (conservative)
      max_ingress_bytes: 10737418240  # 10 GB

  - name: r2
    # ... endpoint, credentials, quota as above
    usage_limits:
      max_api_requests: 1000000
      # R2 has no egress fees, but you can still set limits
```

When a backend hits a usage limit, reads fail over to replicas on other backends and writes overflow to backends that still have headroom.

{{% notice tip %}}
Set limits slightly below the actual free-tier cap to give yourself a safety margin. The orchestrator's adaptive flushing shortens the tracking interval as limits approach, but a small buffer avoids edge cases.
{{% /notice %}}

## Step 5: Create a Virtual Bucket and Client Credentials

Your applications do not connect with the provider credentials above. Instead, you create a **virtual bucket** with its own set of credentials. These are standard S3 access key / secret key pairs that the orchestrator uses to authenticate your clients via AWS SigV4 signing.

Generate a credential pair:

```bash
echo "Access Key: $(openssl rand -hex 10 | tr '[:lower:]' '[:upper:]')"
echo "Secret Key: $(openssl rand -base64 30)"
```

Add them to your config:

```yaml
buckets:
  - name: myapp
    credentials:
      - access_key_id: "YOUR_GENERATED_ACCESS_KEY"
        secret_access_key: "YOUR_GENERATED_SECRET_KEY"
```

You can create multiple virtual buckets with independent credentials for different applications or teams. Each bucket is isolated - clients can only access objects in their own bucket.

## Step 6: Connect Your Application

Point your S3 client at the orchestrator's endpoint using the virtual bucket credentials from Step 5. Any S3-compatible tool or SDK works with no modifications.

```bash
# AWS CLI
aws configure set aws_access_key_id YOUR_GENERATED_ACCESS_KEY
aws configure set aws_secret_access_key YOUR_GENERATED_SECRET_KEY
aws configure set default.endpoint_url http://orchestrator-host:9000
aws configure set default.region us-east-1  # any valid region works

# Upload a file
aws s3 cp myfile.txt s3://myapp/myfile.txt

# List objects
aws s3 ls s3://myapp/
```

```bash
# rclone
rclone config create s3orch s3 \
  provider=Other \
  access_key_id=YOUR_GENERATED_ACCESS_KEY \
  secret_access_key=YOUR_GENERATED_SECRET_KEY \
  endpoint=http://orchestrator-host:9000

rclone copy myfile.txt s3orch:myapp/
```

```python
# Python (boto3)
import boto3

s3 = boto3.client('s3',
    endpoint_url='http://orchestrator-host:9000',
    aws_access_key_id='YOUR_GENERATED_ACCESS_KEY',
    aws_secret_access_key='YOUR_GENERATED_SECRET_KEY')

s3.upload_file('myfile.txt', 'myapp', 'myfile.txt')
```

Your application has no knowledge of OCI, B2, R2, or any backend. It talks to a single S3 endpoint and the orchestrator handles the rest.

## Step 7: Choose a Routing Strategy

The `spread` strategy distributes writes across backends, which helps keep usage balanced across all providers:

```yaml
routing_strategy: spread
```

Alternatively, `pack` fills one backend before moving to the next, which can be useful if one provider has more generous limits.

## Step 8: Monitor Usage

Use the web dashboard or Prometheus metrics to track how close each backend is to its limits:

- **Storage quota**: `s3proxy_backend_used_bytes` vs `s3proxy_backend_quota_bytes`
- **API requests**: `s3proxy_backend_api_requests_total`
- **Egress/Ingress**: `s3proxy_backend_egress_bytes_total` / `s3proxy_backend_ingress_bytes_total`

The dashboard shows per-backend quota bars and monthly usage charts so you can see at a glance how much headroom remains.

## Adding More Providers

To expand your pool, add another backend with its own quota and usage limits. No existing configuration needs to change. The orchestrator picks up new backends on configuration reload.

{{% notice tip %}}
Enable [replication](../../docs/user-guide.md) with a factor of 2 or more so that objects are copied across providers. This gives you redundancy and allows reads to fail over when one backend's usage limits are reached.
{{% /notice %}}

## Important Notes

- Quotas are enforced atomically on every write - you will never accidentally exceed a backend's limit
- Usage limits reset monthly - the orchestrator tracks the current billing period automatically
- Adding or removing backends does not require downtime - reload the configuration and the orchestrator adjusts routing immediately
- Replication copies count against the destination backend's quota and usage limits, so factor that into your calculations
- Backend credentials (from the provider) and client credentials (for your virtual buckets) are completely separate - rotating one does not affect the other
