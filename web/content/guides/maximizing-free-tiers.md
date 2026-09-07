---
title: "Maximizing Free Tiers"
description: "Combine the free tiers of several S3-compatible providers into one endpoint: account setup, per-backend quotas, and replication across them."
weight: 3
---


This guide walks through combining free-tier object storage from multiple cloud providers into a single, larger storage pool using S3 Orchestrator, from creating provider accounts to connecting your first application.

## Overview

Most S3-compatible providers offer a free tier with a limited amount of storage and API requests. Individually these allocations are small, but S3 Orchestrator lets you stack them behind a single endpoint. The orchestrator handles routing writes to backends with available quota, overflowing to the next backend when one fills up.

The key tools for staying within free tiers are **per-backend quotas** and **usage limits**. Quotas cap stored bytes so you never exceed a provider's free storage allowance. Usage limits cap monthly API requests, egress, and ingress so you avoid overage charges on metered dimensions.

![Seven cloud backends with a free-tier configuration](/docs/images/multi-cloud-setup.png?classes=lightbox)

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
  driver: postgres
  host: "haproxy-postgres.service.consul"
  port: 5433
  database: "s3_orchestrator"
  user: "{{ .Data.data.db_username }}"
  password: "{{ .Data.data.db_password }}"
  ssl_mode: "require"
  max_conns: 50
  min_conns: 10
  max_conn_lifetime: "5m"

backends:
  - name: "oci"
    endpoint: "{{ .Data.data.oci_s3_endpoint }}"
    region: "{{ .Data.data.oci_s3_region }}"
    bucket: "{{ .Data.data.oci_s3_bucket }}"
    access_key_id: "{{ .Data.data.oci_s3_access_key }}"
    secret_access_key: "{{ .Data.data.oci_s3_secret_key }}"
    force_path_style: true
    quota_bytes: 18000000000          # 20 GB free tier, kept under the cap
    api_request_limit: 50000          # OCI does not split requests by class
    egress_byte_limit: 1000000000000  # 1 TB of the tenancy-wide 10 TB

  - name: "r2"
    endpoint: "{{ .Data.data.r2_s3_endpoint }}"
    region: "auto"
    bucket: "{{ .Data.data.r2_s3_bucket }}"
    access_key_id: "{{ .Data.data.r2_s3_access_key }}"
    secret_access_key: "{{ .Data.data.r2_s3_secret_key }}"
    force_path_style: true
    quota_bytes: 10000000000
    unmetered: [DeleteObject, DeleteObjects, AbortMultipartUpload]
    request_limits:
      - name: class_a
        operations: [PutObject, CopyObject, ListObjects, ListObjectsV2, CreateMultipartUpload, UploadPart, CompleteMultipartUpload, GetParts]
        limit: 1000000
      - name: class_b
        operations: [GetObject, HeadObject]
        limit: 10000000

  - name: "b2"
    endpoint: "{{ .Data.data.b2_s3_endpoint }}"
    region: "{{ .Data.data.b2_s3_region }}"
    bucket: "{{ .Data.data.b2_s3_bucket }}"
    access_key_id: "{{ .Data.data.b2_s3_access_key }}"
    secret_access_key: "{{ .Data.data.b2_s3_secret_key }}"
    force_path_style: true
    # Class A, B and C transactions are free, so this backend carries no
    # request budget. The egress allowance is 3x the average monthly stored
    # bytes, so it tracks the quota rather than a fixed monthly figure.
    quota_bytes: 10000000000
    egress_byte_limit: 30000000000

  - name: "e2"
    endpoint: "{{ .Data.data.e2_s3_endpoint }}"
    region: "{{ .Data.data.e2_s3_region }}"
    bucket: "{{ .Data.data.e2_s3_bucket }}"
    access_key_id: "{{ .Data.data.e2_s3_access_key }}"
    secret_access_key: "{{ .Data.data.e2_s3_secret_key }}"
    force_path_style: true
    disable_checksum: true
    quota_bytes: 10000000000
    egress_byte_limit: 30000000000  # 3x active stored bytes

  - name: "ibm"
    endpoint: "{{ .Data.data.ibm_s3_endpoint }}"
    region: "{{ .Data.data.ibm_s3_region }}"
    bucket: "{{ .Data.data.ibm_s3_bucket }}"
    access_key_id: "{{ .Data.data.ibm_s3_access_key }}"
    secret_access_key: "{{ .Data.data.ibm_s3_secret_key }}"
    force_path_style: true
    # IBM's Class B is "GET and all others", so deletes and aborts charge
    # there rather than being free as they are on R2 and GCS.
    quota_bytes: 5000000000
    egress_byte_limit: 5000000000
    request_limits:
      - name: class_a
        operations: [PutObject, CopyObject, ListObjects, ListObjectsV2, CreateMultipartUpload, UploadPart, CompleteMultipartUpload]
        limit: 2000
      - name: class_b
        operations: [GetObject, HeadObject, GetParts, DeleteObject, DeleteObjects, AbortMultipartUpload]
        limit: 20000

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
    # GCS bills uploads and listings from the Class A allowance, reads from a
    # far larger Class B one, and does not bill deletes at all. A single
    # api_request_limit charges all three against the smallest of them, which
    # takes the backend out of service with its read allowance untouched.
    quota_bytes: 5000000000
    egress_byte_limit: 100000000000  # 100 GB from North America
    unmetered: [DeleteObject, DeleteObjects, AbortMultipartUpload]
    request_limits:
      - name: class_a
        operations: [PutObject, CopyObject, CreateMultipartUpload, UploadPart, CompleteMultipartUpload, ListObjects, ListObjectsV2]
        limit: 5000
      - name: class_b
        operations: [GetObject, HeadObject, GetParts]
        limit: 50000

  - name: "g3"
    endpoint: "{{ .Data.data.g3_s3_endpoint }}"
    region: "us-east-1"
    bucket: "{{ .Data.data.g3_s3_bucket }}"
    access_key_id: "{{ .Data.data.g3_s3_access_key }}"
    secret_access_key: "{{ .Data.data.g3_s3_secret_key }}"
    force_path_style: true
    quota_bytes: 16106127360

circuit_breaker:
  failure_threshold: 3
  open_timeout: 15s
  cache_ttl: 60s

backend_circuit_breaker:
  enabled: true
  failure_threshold: 3
  open_timeout: 15s

replication:
  factor: 2

# The write places both copies itself, so the replicator never reads an object
# back to make one - no GET and no source egress per replica.
write_path:
  parallel_copies:
    enabled: true

# Compressed bytes are what count against quota_bytes and both byte limits.
compression:
  enabled: true
  level: "default"
  min_size: 4096
  min_ratio: 0.95

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
  session_secret: "{{ .Data.data.ui_session_secret }}"
  force_secure_cookies: true   # unconditionally sets Secure on session cookies; alternative is to let the orchestrator detect TLS via X-Forwarded-Proto from a trusted_proxies CIDR — see docs/security-hardening.md

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

See the [Admin Guide](../../docs/backends/) for more details.
{{% /notice %}}

## Prerequisites

- S3 Orchestrator installed and running (see the [Quickstart](../../docs/quickstart/))
- A PostgreSQL database for the orchestrator's metadata
- Accounts on two or more S3-compatible providers with free-tier allocations

## Step 1: Identify Your Free-Tier Allowances

Check each provider's free-tier limits. The allowances below were checked in September 2026:

| Provider | Free storage | Free requests | Free egress |
|----------|-------------|---------------|-------------|
| [Cloudflare R2](https://developers.cloudflare.com/r2/pricing/) | 10 GB | 1,000,000 Class A, 10,000,000 Class B; deletes and aborts unbilled | Unlimited |
| [Oracle Cloud (OCI)](https://docs.oracle.com/en-us/iaas/Content/FreeTier/freetier_topic-Always_Free_Resources.htm) | 20 GB across all storage tiers | 50,000, not split by class | 10 TB/mo, shared by the whole tenancy |
| [Backblaze B2](https://www.backblaze.com/cloud-storage/pricing) | 10 GB | Class A, B and C transactions are all free | 3x average monthly stored bytes |
| [IDrive e2](https://e2help.idrive.com/hc/en-us/articles/10910766716573-IDrive-e2-pricing-policies) | 10 GB | No request charges | 3x active stored bytes |
| [Synology C2](https://c2.synology.com/en-global/pricing/onestorage) | 15 GB | No request charges | 15 GB/mo |
| [Tigris](https://www.tigrisdata.com/pricing/) | 5 GB | 10,000 Class A, 100,000 Class B; deletes free | Unlimited |
| [Google Cloud (GCS)](https://docs.cloud.google.com/free/docs/free-cloud-features) | 5 GB, us-east1/us-west1/us-central1 only | 5,000 Class A, 50,000 Class B; deletes free | 100 GB/mo from North America |
| [IBM Cloud](https://cloud.ibm.com/docs/cloud-object-storage?topic=cloud-object-storage-faq-provision) | 5 GB Smart Tier | 2,000 Class A, 20,000 Class B | 5 GB/mo public egress |
| [Supabase](https://supabase.com/pricing) | 1 GB | No request charges | 5 GB/mo, shared across the organization |
| [g3](https://g3.munchbox.cc) (Gmail + Drive) | 15 GB per Google account | Drive API per-minute quotas only | No monthly cap |

Together those come to 96 GB of combined storage behind a single S3 endpoint.

That is the raw figure, and it is only what you can store if every object exists once. **Replication divides it.** At a factor of 2 the same 96 GB holds 48 GB of distinct objects, since every one is written twice; at 3 it holds 32 GB. Decide the factor before sizing the pool, because it is the difference between a 96 GB answer and a 32 GB one. Compression pushes back the other way — see Step 4 — but by an amount that depends entirely on what you store, so plan on the raw division and treat compression as headroom you earn rather than capacity you can count on.

Three things in that table decide how the backend is configured:

**Requests are metered by class, not in total.** GCS bills uploads and listings from the Class A allowance and reads from a Class B one twenty times larger, and does not bill deletes at all. IBM charges deletes to Class B, because its Class B is "GET and all others". B2 charges nothing for any of them. A single `api_request_limit` charges every operation against one number, so it has to be set to the strictest class, which takes the whole backend out of service while the looser allowances sit unused. Declare the grouping with `request_limits` and `unmetered` instead; see [backends.md](../../docs/backends/) for the syntax and the operation vocabulary.

**Two of the egress allowances scale with what is stored.** B2 and IDrive e2 give away a multiple of the bytes actually held, so a fixed `egress_byte_limit` is only safe at the storage level it was computed for. A backend holding 2 GB on B2 has a 6 GB allowance, not the 30 GB a full 10 GB backend would earn.

**Some allowances are narrower than the provider's headline.** The GCS free tier applies only to the three US regions listed; a bucket anywhere else bills from the first operation. The OCI egress allowance covers the entire tenancy, so compute instances draw from the same 10 TB. The g3 storage is a Google account quota shared with Gmail and Photos.

{{% notice tip %}}
**[g3](https://github.com/afreidah/g3)** is an S3-compatible gateway that uses Google Drive for object data and Gmail for metadata. Each free Google account provides 15 GB of storage. g3 runs as a service in your infrastructure and presents a standard S3 API that S3 Orchestrator connects to like any other backend. See the [g3 project website](https://g3.munchbox.cc) for setup instructions.
{{% /notice %}}

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
    quota_bytes: 18000000000          # 20 GB free tier, kept under the cap
    api_request_limit: 50000          # OCI does not split requests by class
    egress_byte_limit: 1000000000000  # 1 TB of the tenancy-wide 10 TB

  - name: "r2"
    endpoint: "https://<account-id>.r2.cloudflarestorage.com"
    region: "auto"
    bucket: "my-bucket"
    access_key_id: "${R2_ACCESS_KEY}"
    secret_access_key: "${R2_SECRET_KEY}"
    force_path_style: true
    quota_bytes: 10000000000
    unmetered: [DeleteObject, DeleteObjects, AbortMultipartUpload]
    request_limits:
      - name: class_a
        operations: [PutObject, CopyObject, ListObjects, ListObjectsV2, CreateMultipartUpload, UploadPart, CompleteMultipartUpload, GetParts]
        limit: 1000000
      - name: class_b
        operations: [GetObject, HeadObject]
        limit: 10000000

  - name: "b2"
    endpoint: "https://s3.<region>.backblazeb2.com"
    region: "us-west-004"
    bucket: "my-bucket"
    access_key_id: "${B2_ACCESS_KEY}"
    secret_access_key: "${B2_SECRET_KEY}"
    force_path_style: true
    # Class A, B and C transactions are free, so this backend carries no
    # request budget. The egress allowance is 3x the average monthly stored
    # bytes, so it tracks the quota rather than a fixed monthly figure.
    quota_bytes: 10000000000
    egress_byte_limit: 30000000000

  - name: "e2"
    endpoint: "https://<endpoint>.e2.cloudstorage.com"
    region: "us-east-005"
    bucket: "my-bucket"
    access_key_id: "${E2_ACCESS_KEY}"
    secret_access_key: "${E2_SECRET_KEY}"
    force_path_style: true
    disable_checksum: true
    quota_bytes: 10000000000
    egress_byte_limit: 30000000000  # 3x active stored bytes

  - name: "ibm"
    endpoint: "https://s3.<region>.cloud-object-storage.appdomain.cloud"
    region: "us-south"
    bucket: "my-bucket"
    access_key_id: "${IBM_ACCESS_KEY}"
    secret_access_key: "${IBM_SECRET_KEY}"
    force_path_style: true
    # IBM's Class B is "GET and all others", so deletes and aborts charge
    # there rather than being free as they are on R2 and GCS.
    quota_bytes: 5000000000
    egress_byte_limit: 5000000000
    request_limits:
      - name: class_a
        operations: [PutObject, CopyObject, ListObjects, ListObjectsV2, CreateMultipartUpload, UploadPart, CompleteMultipartUpload]
        limit: 2000
      - name: class_b
        operations: [GetObject, HeadObject, GetParts, DeleteObject, DeleteObjects, AbortMultipartUpload]
        limit: 20000

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
    quota_bytes: 5000000000
    egress_byte_limit: 100000000000  # 100 GB from North America
    # GCS bills uploads and listings from the Class A allowance, reads from a
    # far larger Class B one, and does not bill deletes at all. A single
    # api_request_limit charges all three against the smallest of them, which
    # takes the backend out of service with its read allowance untouched.
    unmetered: [DeleteObject, DeleteObjects, AbortMultipartUpload]
    request_limits:
      - name: class_a
        operations: [PutObject, CopyObject, CreateMultipartUpload, UploadPart, CompleteMultipartUpload, ListObjects, ListObjectsV2]
        limit: 5000
      - name: class_b
        operations: [GetObject, HeadObject, GetParts]
        limit: 50000

  # g3 uses Google Drive + Gmail as storage via an S3-compatible proxy.
  # See https://github.com/afreidah/g3 for setup.
  - name: "g3"
    endpoint: "http://g3-proxy.service.consul:9001"
    region: "us-east-1"
    bucket: "my-bucket"
    access_key_id: "${G3_ACCESS_KEY}"
    secret_access_key: "${G3_SECRET_KEY}"
    force_path_style: true
    quota_bytes: 16106127360
```

When a backend hits a usage limit, reads fail over to replicas on other backends and writes overflow to backends that still have headroom.

{{% notice tip %}}
Set limits slightly below the actual free-tier cap to give yourself a safety margin. The orchestrator's adaptive flushing shortens the tracking interval as limits approach, but a small buffer avoids edge cases.
{{% /notice %}}

## Step 4: Spend Less of Every Allowance

Quotas and usage limits stop you exceeding an allowance. Two further settings reduce what you consume of it in the first place, and on free tiers both are worth more than they are on paid storage.

### Compression: fewer bytes stored and moved

```yaml
compression:
  enabled: true
  level: "default"
  min_size: 4096
  min_ratio: 0.95
```

Objects are stored as chunked zstd, so a backend holds the compressed size and that is what counts against `quota_bytes`. The same reduction applies to every byte crossing the wire, so `ingress_byte_limit` on the write and `egress_byte_limit` on every later read shrink with it. Three allowances for one setting.

How much depends entirely on your data: text, JSON, logs and most documents compress several times over, while media files and anything already compressed do not move at all. Objects below `min_size` are stored verbatim because a seek table costs more than a small object saves, and anything that fails to shrink past `min_ratio` is stored verbatim too — so incompressible data costs you the attempt and nothing else. Range reads stay proportional to the bytes requested rather than the object size, because each chunk is an independently decodable frame.

### Write-placed copies: no read to make a replica

```yaml
replication:
  factor: 2

write_path:
  parallel_copies:
    enabled: true
```

With replication on, a second copy has to come from somewhere. By default the replicator makes it by reading the object back off the backend that holds it and writing it to another — which charges the source a **GET and its egress**, then the target a PUT. On a free tier that is exactly the wrong shape: reads are what the small Class A/Class B allowances and the egress caps meter.

`parallel_copies` has the write upload to both backends itself, from bytes it already has in hand. The PUT on the target is unavoidable either way, but the GET and the source egress disappear entirely. For a factor of 2 that removes one read per object from your API budget, and the bytes of every replica from an egress allowance — on IBM's 5 GB/month egress or GCS's 5,000 Class A calls, that is the difference between a working backend and one that goes read-only halfway through the month.

The cost is that both copies are uploaded during the write rather than spread over replicator cycles, so peak write bandwidth roughly doubles. For free-tier setups, where the constraint is a monthly allowance rather than throughput, that is usually the right trade.

{{% notice tip %}}
Both settings compound: compression shrinks the object, and the write then places the compressed copies without reading anything back. Together they cut what replication costs you on every metered dimension a provider bills.
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

- **Storage quota**: `s3o_quota_bytes_used{backend}` against `s3o_quota_bytes_limit{backend}`, with `s3o_quota_bytes_available{backend}` as the headroom directly
- **API requests**: `s3o_usage_api_requests{backend}` for the month's total, and `s3o_usage_pool_requests{backend,pool}` against `s3o_usage_pool_limit{backend,pool}` for each request class you declared with `request_limits`
- **Egress / ingress**: `s3o_usage_egress_bytes{backend}` and `s3o_usage_ingress_bytes{backend}`
- **Hitting a cap**: `s3o_usage_limit_rejections_total{operation,direction}` counts operations turned away because a backend was out of allowance, and `s3o_quota_claims_declined_total{backend}` counts writes a backend refused for want of space

The dashboard shows per-backend quota bars and monthly usage charts so you can see at a glance how much headroom remains.

The per-class metrics are the ones to watch on providers that meter by class. A backend can be nowhere near its overall request count while its Class A allowance is exhausted — which on GCS, at 5,000 uploads a month against 50,000 reads, is the failure mode you will meet first.

## Adding More Providers

To expand your pool, add another backend with its own quota and usage limits. No existing configuration needs to change. The orchestrator picks up new backends on configuration reload.

{{% notice tip %}}
Enable [replication](../../docs/replication/) with a factor of 2 or more so that objects are copied across providers. This gives you redundancy and allows reads to fail over when one backend's usage limits are reached. Remember that it also halves your usable capacity - see the note under Step 1.
{{% /notice %}}

## Reduce API Calls with the Object Data Cache

If your workload reads the same objects frequently, enable the object data cache to serve repeated GETs from memory instead of hitting backends:

```yaml
cache:
  enabled: true
  max_size: "256MB"
  max_object_size: "10MB"
  ttl: "5m"
```

Each cache hit avoids one backend API request and the associated egress, which directly extends your free-tier headroom. The cache is especially effective for providers with low API request limits (e.g., OCI at 50,000/mo or GCS at 5,000/mo).

{{% notice tip %}}
Size `max_object_size` to match your typical object sizes. If most of your objects are small config files or thumbnails, a 1-5 MB limit keeps the cache efficient. Large objects that would consume the entire cache are better left to direct backend reads.
{{% /notice %}}

## Important Notes

- Quotas are enforced atomically on every write - you will never accidentally exceed a backend's limit
- Usage limits reset monthly - the orchestrator tracks the current billing period automatically
- Adding or removing backends does not require downtime - reload the configuration and the orchestrator adjusts routing immediately
- Replication copies count against the destination backend's quota and usage limits, so factor that into your calculations
- Backend credentials (from the provider) and client credentials (for your virtual buckets) are completely separate - rotating one does not affect the other
