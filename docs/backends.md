---
title: "Backends"
linkTitle: "Backends"
weight: 23
---

# Backends

Configuration, routing strategies, multi-backend topologies, and the provider quick-reference table.

## Backend configuration


Each backend is an S3-compatible storage service with its own credentials and optional quota.

```yaml
backends:
  - name: "oci"
    endpoint: "https://namespace.compat.objectstorage.us-phoenix-1.oraclecloud.com"
    region: "us-phoenix-1"
    bucket: "my-oci-bucket"
    access_key_id: "${OCI_ACCESS_KEY}"
    secret_access_key: "${OCI_SECRET_KEY}"
    force_path_style: true
    quota_bytes: 21474836480     # 20 GB
```

**Endpoint URLs by provider:**

| Provider | Endpoint format | `force_path_style` |
|----------|----------------|-------------------|
| OCI Object Storage | `https://<namespace>.compat.objectstorage.<region>.oraclecloud.com` | `true` |
| Backblaze B2 | `https://s3.<region>.backblazeb2.com` | `true` |
| AWS S3 | `https://s3.<region>.amazonaws.com` | `false` |
| MinIO | `http://<host>:9000` | `true` |
| Wasabi | `https://s3.<region>.wasabisys.com` | `true` |

**Quota:** Set `quota_bytes` to limit how much data a backend can hold. Set to `0` or omit for unlimited. Quota is tracked in PostgreSQL and updated atomically with every write/delete. Note that multipart uploads do not reserve quota upfront — temporary parts consume backend storage without being counted against the quota until `CompleteMultipartUpload` records the final object size. A client uploading many large parts could temporarily exceed a backend's quota before completion.

**Max object size:** Some providers impose per-object size limits (e.g. Supabase rejects uploads over 50 MB with 413 EntityTooLarge). Set `max_object_size` to prevent the orchestrator from routing writes, rebalance moves, or replication copies to a backend when the object exceeds the limit:

```yaml
    max_object_size: 52428800    # 50 MB (0 = unlimited)
```

**Usage limits:** Optional monthly caps on API requests, egress, and ingress per backend:

```yaml
    api_request_limit: 20000     # monthly API calls (0 = unlimited)
    egress_byte_limit: 1073741824  # 1 GB monthly egress (0 = unlimited)
    ingress_byte_limit: 0        # unlimited ingress
```

When a backend exceeds a usage limit, writes overflow to the next eligible backend. Limits reset each month automatically.

**Unsigned payload:** By default, uploads stream directly to backends without buffering the entire body in memory. The AWS SDK normally buffers the request body to compute a SigV4 payload hash (SHA-256), but the orchestrator uses `UNSIGNED-PAYLOAD` to skip this. Without streaming, large uploads (multipart completion, replication) can cause out-of-memory kills.

For HTTPS endpoints, unsigned payload is enabled by default. For plain HTTP endpoints, it is auto-disabled unless explicitly set — AWS S3 rejects unsigned payloads over HTTP, but most S3-compatible backends (MinIO, R2, etc.) accept them. Set `unsigned_payload: true` on HTTP backends to enable streaming:

```yaml
    unsigned_payload: true   # stream uploads without buffering (auto-enabled for HTTPS)
```

Set `unsigned_payload: false` to force payload hashing. This buffers the entire object in memory before uploading — only use this if you have a specific compliance requirement for end-to-end payload integrity independent of TLS.

**Disable checksum:** AWS SDK v2 defaults to sending streaming checksums (CRC64NVME) on uploads. Some S3-compatible providers — notably Google Cloud Storage — reject these with `SignatureDoesNotMatch`. Set `disable_checksum: true` on backends that don't support the AWS checksum headers:

```yaml
    disable_checksum: true   # required for GCS HMAC interoperability
```

This sets the SDK's `RequestChecksumCalculation` and `ResponseChecksumValidation` to `WhenRequired`, disabling automatic checksum injection without affecting SigV4 request signing.

**Strip SDK headers:** AWS SDK v2 adds headers (`amz-sdk-invocation-id`, `amz-sdk-request`, `accept-encoding`) and a query parameter (`x-id`) that are included in the SigV4 signed header set. Google Cloud Storage does not include these when verifying the signature, causing `SignatureDoesNotMatch` errors. Set `strip_sdk_headers: true` to remove them before request signing:

```yaml
    strip_sdk_headers: true   # required for GCS HMAC interoperability
```

For GCS backends, you typically need both `disable_checksum: true` and `strip_sdk_headers: true`:

```yaml
  - name: "gcs"
    endpoint: "https://storage.googleapis.com"
    region: "auto"
    bucket: "my-bucket"
    access_key_id: "GOOG..."
    secret_access_key: "..."
    force_path_style: true
    disable_checksum: true
    strip_sdk_headers: true
```

**Credential source:** `credential_source` selects how the orchestrator obtains credentials for the backend. Default is `static`, which uses the `access_key_id` / `secret_access_key` fields above. Set to `default_chain` to delegate to the AWS SDK's default credential chain (env vars, EC2 IMDS, SSO, `~/.aws/credentials`, STS assume-role). When `default_chain` is set, the two key fields must be omitted — leaving stale keys behind is rejected at validation so they cannot silently shadow the SDK-resolved credentials.

Use `default_chain` when:

- The orchestrator runs on an EC2 instance with an IAM role attached (IMDS-vended credentials rotate every ~6 hours and cannot be tracked by YAML).
- Local development uses SSO (`aws sso login`) instead of long-lived keys.
- You want the SDK to resolve credentials via STS assume-role chains.

```yaml
  - name: "aws-prod"
    endpoint: "https://s3.amazonaws.com"
    region: "us-east-1"
    bucket: "my-prod-bucket"
    credential_source: "default_chain"
    # access_key_id / secret_access_key intentionally omitted
```

Note: the config loader already expands `${ENV_VAR}` references at load time, so `access_key_id: ${AWS_ACCESS_KEY_ID}` covers the env-var case under `credential_source: static`. Use `default_chain` for credential sources the loader cannot reach (IMDS, SSO, STS) and for cases where refresh matters.


## Routing strategy


Controls how the orchestrator selects a backend when writing new objects.

```yaml
routing_strategy: "pack"       # "pack" or "spread" (default: pack)
```

- **pack** (default) — fills the first backend in config order until its quota is full, then overflows to the next. Best for stacking free-tier allocations sequentially.
- **spread** — places each object on the backend with the lowest utilization ratio (`(bytes_used + orphan_bytes) / bytes_limit`). Best for distributing storage evenly across backends.

Both strategies respect quota limits and usage limits — full or over-limit backends are always skipped.


## Multi-backend configurations


### Single backend with quota

The simplest setup. One backend with a byte cap:

```yaml
backends:
  - name: "oci"
    endpoint: "https://namespace.compat.objectstorage.region.oraclecloud.com"
    region: "us-phoenix-1"
    bucket: "my-bucket"
    access_key_id: "${OCI_KEY}"
    secret_access_key: "${OCI_SECRET}"
    force_path_style: true
    quota_bytes: 21474836480     # 20 GB
```

### Multiple backends with quotas (pack routing)

Stack multiple free-tier allocations. With the default `routing_strategy: "pack"`, when one backend fills up, writes overflow to the next. Use `routing_strategy: "spread"` instead to distribute objects evenly by utilization ratio:

```yaml
backends:
  - name: "oci-free"
    endpoint: "https://namespace.compat.objectstorage.region.oraclecloud.com"
    region: "us-phoenix-1"
    bucket: "free-tier-bucket"
    access_key_id: "${OCI_KEY}"
    secret_access_key: "${OCI_SECRET}"
    force_path_style: true
    quota_bytes: 21474836480     # 20 GB (OCI free tier)

  - name: "b2-free"
    endpoint: "https://s3.us-west-002.backblazeb2.com"
    region: "us-west-002"
    bucket: "free-tier-bucket"
    access_key_id: "${B2_KEY}"
    secret_access_key: "${B2_SECRET}"
    force_path_style: true
    quota_bytes: 10737418240     # 10 GB (B2 free tier)
```

This gives you 30 GB of combined storage across two providers.

### Multiple backends without quotas (requires replication or spread)

When all backends are unlimited and using the default `pack` routing, only the first backend would receive writes. To distribute data, either set `replication.factor >= 2` to replicate across backends, or use `routing_strategy: "spread"` to distribute writes by utilization ratio.

```yaml
backends:
  - name: "oci"
    endpoint: "https://namespace.compat.objectstorage.region.oraclecloud.com"
    region: "us-phoenix-1"
    bucket: "bucket-a"
    access_key_id: "${OCI_KEY}"
    secret_access_key: "${OCI_SECRET}"
    force_path_style: true
    # no quota_bytes — unlimited

  - name: "aws"
    endpoint: "https://s3.us-east-1.amazonaws.com"
    region: "us-east-1"
    bucket: "bucket-b"
    access_key_id: "${AWS_KEY}"
    secret_access_key: "${AWS_SECRET}"
    # no quota_bytes — unlimited

replication:
  factor: 2
```

**Validation rule:** You cannot mix unlimited and quota-limited backends. Either all backends have `quota_bytes` set (quota routing) or all are unlimited (replication or spread routing required).


## Usage limits

## Usage Limits

Per-backend monthly limits for API requests, egress bytes, and ingress bytes. Set any limit to `0` (or omit it) for unlimited. Limits reset naturally each month — the usage tracking table is keyed by `YYYY-MM` period.

**Enforcement behavior:**

- **Writes** (PutObject, CopyObject, CreateMultipartUpload, UploadPart) — backends over their limits are excluded from selection; writes overflow to the next eligible backend. If all backends are over-limit, the orchestrator returns `507 InsufficientStorage`.
- **Reads** (GetObject, HeadObject) — over-limit backends are skipped; the orchestrator tries replicas. Returns `429 SlowDown` only when *all* copies of the object are on over-limit backends.
- **Deletes** (DeleteObject, DeleteObjects, AbortMultipartUpload) — always allowed regardless of limits.

Effective usage is computed as `DB baseline + unflushed counters + proposed operation`, so enforcement stays accurate between flush/refresh cycles without double-counting. The flush interval is configurable (default 30s) and can adaptively shorten when backends approach their limits. For multi-instance deployments, optional [Redis shared counters](#usage-counters) eliminate the cross-instance blind spot between flushes.
