---
title: "Maximizing Free Tiers"
weight: 3
---


This guide walks through combining free-tier object storage from multiple cloud providers into a single, larger storage pool using S3 Orchestrator, from creating provider accounts to connecting your first application.

## Overview

Most S3-compatible providers offer a free tier with a limited amount of storage and API requests. Individually these allocations are small, but S3 Orchestrator lets you stack them behind a single endpoint. The orchestrator handles routing writes to backends with available quota, overflowing to the next backend when one fills up.

The key tools for staying within free tiers are **per-backend quotas** and **usage limits**. Quotas cap stored bytes so you never exceed a provider's free storage allowance. Usage limits cap monthly API requests, egress, and ingress so you avoid overage charges on metered dimensions.

![Six cloud backends with a free-tier configuration](/docs/images/free-tier-5-cloud-setup.png?classes=lightbox)

Below is the configuration used to run the setup shown above. Credentials are injected via Vault templates, but you can substitute environment variables or literal values.

```yaml
server:
  listen_addr: "0.0.0.0:9000"
  backend_timeout: "5m"
  shutdown_delay: "5s"

routing_strategy: "spread"

buckets:
  - name: "unified"
    credentials:
      - access_key_id: "{{ .Data.data.access_key }}"
        secret_access_key: "{{ .Data.data.secret_key }}"

database:
  host: "haproxy-postgres.service.consul"
  port: 5433
  database: "s3_orchestrator"
  user: "{{ .Data.data.db_username }}"
  password: "{{ .Data.data.db_password }}"
  ssl_mode: "require"
  max_conns: 10
  min_conns: 5
  max_conn_lifetime: "5m"

backends:
  - name: "oci"
    endpoint: "{{ .Data.data.oci_s3_endpoint }}"
    region: "{{ .Data.data.oci_s3_region }}"
    bucket: "{{ .Data.data.oci_s3_bucket }}"
    access_key_id: "{{ .Data.data.oci_s3_access_key }}"
    secret_access_key: "{{ .Data.data.oci_s3_secret_key }}"
    force_path_style: true
    quota_bytes: 10737418240
    api_request_limit: 50000
    egress_byte_limit: 10737418240

  - name: "r2"
    endpoint: "{{ .Data.data.r2_s3_endpoint }}"
    region: "auto"
    bucket: "{{ .Data.data.r2_s3_bucket }}"
    access_key_id: "{{ .Data.data.r2_s3_access_key }}"
    secret_access_key: "{{ .Data.data.r2_s3_secret_key }}"
    force_path_style: true
    quota_bytes: 10737418240
    api_request_limit: 1000000

  - name: "b2"
    endpoint: "{{ .Data.data.b2_s3_endpoint }}"
    region: "{{ .Data.data.b2_s3_region }}"
    bucket: "{{ .Data.data.b2_s3_bucket }}"
    access_key_id: "{{ .Data.data.b2_s3_access_key }}"
    secret_access_key: "{{ .Data.data.b2_s3_secret_key }}"
    force_path_style: true
    quota_bytes: 10737418240
    egress_byte_limit: 32212254720
    api_request_limit: 75000

  - name: "e2"
    endpoint: "{{ .Data.data.e2_s3_endpoint }}"
    region: "{{ .Data.data.e2_s3_region }}"
    bucket: "{{ .Data.data.e2_s3_bucket }}"
    access_key_id: "{{ .Data.data.e2_s3_access_key }}"
    secret_access_key: "{{ .Data.data.e2_s3_secret_key }}"
    force_path_style: true
    disable_checksum: true
    quota_bytes: 10737418240
    egress_byte_limit: 32212254720
    ingress_byte_limit: 10737418240

  - name: "ibm"
    endpoint: "{{ .Data.data.ibm_s3_endpoint }}"
    region: "{{ .Data.data.ibm_s3_region }}"
    bucket: "{{ .Data.data.ibm_s3_bucket }}"
    access_key_id: "{{ .Data.data.ibm_s3_access_key }}"
    secret_access_key: "{{ .Data.data.ibm_s3_secret_key }}"
    force_path_style: true
    quota_bytes: 5368709120
    egress_byte_limit: 5368709120
    ingress_byte_limit: 5368709120

  # GCS requires three extra settings to work with SigV4 signing.
  # See the note below for details.
  - name: "gcp"
    endpoint: "{{ .Data.data.gcp_s3_endpoint }}"
    region: "{{ .Data.data.gcp_s3_region }}"
    bucket: "{{ .Data.data.gcp_s3_bucket }}"
    access_key_id: "{{ .Data.data.gcp_s3_access_key }}"
    secret_access_key: "{{ .Data.data.gcp_s3_secret_key }}"
    force_path_style: true
    disable_checksum: true
    unsigned_payload: true
    strip_sdk_headers: true
    quota_bytes: 5368709120
    egress_byte_limit: 1073741824
    ingress_byte_limit: 5368709120
    api_request_limit: 5000

circuit_breaker:
  failure_threshold: 3
  open_timeout: 15s
  cache_ttl: 60s

backend_circuit_breaker:
  enabled: true
  failure_threshold: 3
  open_timeout: 15s

encryption:
  enabled: true
  vault:
    address: "https://vault.service.consul:8200"
    token: "${VAULT_TOKEN}"
    key_name: "s3-orchestrator"
    mount_path: "transit"
    ca_cert: "/secrets/vault-ca.pem"

rate_limit:
  enabled: true
  requests_per_sec: 100
  burst: 200
  trusted_proxies:
    - "10.0.0.0/8"
    - "172.16.0.0/12"
    - "192.168.0.0/16"
    - "127.0.0.1/32"

ui:
  enabled: true
  admin_key: "{{ .Data.data.ui_admin_key }}"
  admin_secret: "{{ .Data.data.ui_admin_secret }}"
  force_secure_cookies: true

usage_flush:
  interval: "30s"
  adaptive_enabled: true
  adaptive_threshold: 0.8
  fast_interval: "5s"

telemetry:
  metrics:
    enabled: true
    path: "/metrics"
  tracing:
    enabled: true
    endpoint: "tempo.service.consul:4317"
    insecure: true
    sample_rate: 1.0             # reduce to 0.01–0.1 for production
```

{{% notice note %}}
**Google Cloud Storage** requires three backend-level settings to work correctly:

- **`disable_checksum: true`** — GCS does not support the `x-amz-checksum-*` headers that the AWS SDK sends by default.
- **`unsigned_payload: true`** — GCS does not support `STREAMING-AWS4-HMAC-SHA256-PAYLOAD` chunked signing.
- **`strip_sdk_headers: true`** — AWS SDK v2 adds headers (`amz-sdk-invocation-id`, `amz-sdk-request`, `accept-encoding`) and a query parameter (`x-id`) that GCS does not include when verifying the SigV4 signature, causing `SignatureDoesNotMatch` errors.

See the [Admin Guide](../../docs/admin-guide/#strip-sdk-headers) for more details.
{{% /notice %}}

## Prerequisites

- S3 Orchestrator installed and running (see the [Quickstart](../../docs/quickstart/))
- A PostgreSQL database for the orchestrator's metadata
- Accounts on two or more S3-compatible providers with free-tier allocations

## Step 1: Identify Your Free-Tier Allowances

Check each provider's free-tier limits. Common examples:

| Provider | Free Storage | Free API Requests | Free Egress |
|----------|-------------|-------------------|-------------|
| Oracle Cloud (OCI) | 10 GB | 50,000/mo | 10 GB/mo |
| Cloudflare R2 | 10 GB | 1,000,000/mo | Unlimited |
| Backblaze B2 | 10 GB | 75,000/mo | 30 GB/mo |
| iDrive e2 | 10 GB | Unlimited | 30 GB/mo |
| IBM Cloud | 5 GB | Unlimited | 5 GB/mo |
| Google Cloud (GCS) | 5 GB | 5,000/mo | 1 GB/mo |

With all six providers you get 50 GB of combined storage behind a single S3 endpoint.

{{% notice warning %}}
Free-tier limits change without notice. Always verify current allowances on each provider's pricing page before configuring quotas and usage limits. The numbers listed here are a starting point, not a guarantee.
{{% /notice %}}

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

### Cloudflare R2

1. Log in to the Cloudflare Dashboard
2. Go to **R2 Object Storage** -> **Manage R2 API Tokens**
3. Click **Create API Token**, grant **Object Read & Write** permission
4. Copy the **Access Key ID** and **Secret Access Key**
5. Your S3 endpoint is `https://<account-id>.r2.cloudflarestorage.com` (the account ID is on the R2 overview page)
6. Create a bucket under **R2** -> **Create bucket**

### Backblaze B2

1. Log in to the Backblaze Console
2. Go to **App Keys** -> **Add a New Application Key**
3. Select the bucket (or **All**) and grant **Read and Write** access
4. Copy the **keyID** (this is your access key) and **applicationKey** (this is your secret key)
5. Your S3 endpoint is `https://s3.<region>.backblazeb2.com` (the region is shown on your bucket details page, e.g. `us-west-004`)
6. Create a bucket under **Buckets** -> **Create a Bucket**

### iDrive e2

1. Log in to the iDrive e2 Console
2. Go to **Access Keys** -> **Create Access Key**
3. Copy the **Access Key** and **Secret Key**
4. Your S3 endpoint is `https://<endpoint>.e2.cloudstorage.com` (shown on the dashboard)
5. Create a bucket under **Buckets** -> **Create Bucket**

### IBM Cloud Object Storage

1. Log in to the IBM Cloud Console
2. Create a **Cloud Object Storage** instance (the Lite plan is free)
3. Go to **Service credentials** -> **New credential**, enable **Include HMAC Credential**
4. Expand the credential to find `cos_hmac_keys.access_key_id` and `cos_hmac_keys.secret_access_key`
5. Your S3 endpoint is `https://s3.<region>.cloud-object-storage.appdomain.cloud` (find available regions under **Endpoints**)
6. Create a bucket under **Buckets** -> **Create bucket**, choose a **Standard** storage class

### Google Cloud Storage (GCS)

1. Log in to the Google Cloud Console
2. Go to **Cloud Storage** -> **Settings** -> **Interoperability**
3. If prompted, enable interoperability access for the project
4. Under **Access keys for service accounts**, click **Create a key for a service account** (or use the default)
5. Copy the **Access Key** and **Secret**
6. Your S3 endpoint is `https://storage.googleapis.com`
7. Create a bucket under **Cloud Storage** -> **Buckets** -> **Create**

{{% notice tip %}}
Never commit provider credentials to version control. Use environment variables in your config file with the `${VAR}` syntax, and inject them via systemd `EnvironmentFile`, container secrets, or a secrets manager.
{{% /notice %}}

## Step 3: Configure Backends with Quotas and Usage Limits

Add each provider as a backend in your `config.yaml`. Set `quota_bytes` to match the provider's free storage allowance, and use the usage limit fields to cap API requests, egress, and ingress per billing period.

```yaml
backends:
  - name: "oci"
    endpoint: "https://<namespace>.compat.objectstorage.<region>.oraclecloud.com"
    region: "us-ashburn-1"
    bucket: "my-bucket"
    access_key_id: "${OCI_ACCESS_KEY}"
    secret_access_key: "${OCI_SECRET_KEY}"
    force_path_style: true
    quota_bytes: 10737418240
    api_request_limit: 50000
    egress_byte_limit: 10737418240

  - name: "r2"
    endpoint: "https://<account-id>.r2.cloudflarestorage.com"
    region: "auto"
    bucket: "my-bucket"
    access_key_id: "${R2_ACCESS_KEY}"
    secret_access_key: "${R2_SECRET_KEY}"
    force_path_style: true
    quota_bytes: 10737418240
    api_request_limit: 1000000

  - name: "b2"
    endpoint: "https://s3.<region>.backblazeb2.com"
    region: "us-west-004"
    bucket: "my-bucket"
    access_key_id: "${B2_ACCESS_KEY}"
    secret_access_key: "${B2_SECRET_KEY}"
    force_path_style: true
    quota_bytes: 10737418240
    egress_byte_limit: 32212254720
    api_request_limit: 75000

  - name: "e2"
    endpoint: "https://<endpoint>.e2.cloudstorage.com"
    region: "us-east-005"
    bucket: "my-bucket"
    access_key_id: "${E2_ACCESS_KEY}"
    secret_access_key: "${E2_SECRET_KEY}"
    force_path_style: true
    disable_checksum: true
    quota_bytes: 10737418240
    egress_byte_limit: 32212254720
    ingress_byte_limit: 10737418240

  - name: "ibm"
    endpoint: "https://s3.<region>.cloud-object-storage.appdomain.cloud"
    region: "us-south"
    bucket: "my-bucket"
    access_key_id: "${IBM_ACCESS_KEY}"
    secret_access_key: "${IBM_SECRET_KEY}"
    force_path_style: true
    quota_bytes: 5368709120
    egress_byte_limit: 5368709120
    ingress_byte_limit: 5368709120

  - name: "gcp"
    endpoint: "https://storage.googleapis.com"
    region: "auto"
    bucket: "my-bucket"
    access_key_id: "${GCP_ACCESS_KEY}"
    secret_access_key: "${GCP_SECRET_KEY}"
    force_path_style: true
    disable_checksum: true
    unsigned_payload: true
    strip_sdk_headers: true
    quota_bytes: 5368709120
    egress_byte_limit: 1073741824
    ingress_byte_limit: 5368709120
    api_request_limit: 5000
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

- **Storage quota**: `s3o_backend_used_bytes` vs `s3o_backend_quota_bytes`
- **API requests**: `s3o_backend_api_requests_total`
- **Egress/Ingress**: `s3o_backend_egress_bytes_total` / `s3o_backend_ingress_bytes_total`

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
