---
title: "Authentication"
linkTitle: "Authentication"
weight: 22
---

# Authentication and Credentials

## SigV4 and multi-bucket auth


Each virtual bucket has one or more credential sets. On every request, the orchestrator:

1. Extracts the access key from the SigV4 `Authorization` header, presigned URL query parameters, or token from `X-Proxy-Token`.
2. Looks up which bucket the credential belongs to.
3. Verifies the signature (SigV4 header or presigned query parameters) or token.
4. Validates the URL path bucket matches the authorized bucket.

Three auth methods are supported, checked in order:

1. **AWS SigV4** (recommended) - Standard AWS Signature Version 4 via the `Authorization` header. Compatible with `aws cli`, SDKs, and any S3 client. Signature verification is constant-time: unknown access keys still compute a full HMAC to prevent timing side-channel enumeration. Streaming-payload uploads (`STREAMING-AWS4-HMAC-SHA256-PAYLOAD`, `STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER`, `STREAMING-UNSIGNED-PAYLOAD-TRAILER`) are accepted and the chunk chain is fully validated end-to-end.
2. **Presigned URLs** - SigV4 query-parameter authentication (`X-Amz-Algorithm`, `X-Amz-Credential`, etc.) for time-limited, shareable URLs. Works with any AWS SDK presign client. Maximum expiry: 7 days. Uses the same bucket credentials as normal requests — no additional configuration required.
3. **Legacy token** - Simple `X-Proxy-Token` header for backward compatibility.

Multiple services can share a bucket by each having their own credentials that all map to the same bucket name. Access key IDs must be globally unique across all buckets.

Authentication is always required — every bucket must have at least one credential set.

For client usage examples (AWS CLI, rclone, boto3, Go SDK), see the [User Guide](docs/user-guide.md).
For credential rotation procedures, see [docs/operations.md](operations.md#rotating-client-credentials).

## Bucket configuration


Each bucket defines a virtual namespace with one or more credential sets.

```yaml
buckets:
  - name: "app1-files"
    # max_multipart_uploads: 100  # optional; limit active multipart uploads (0 = unlimited)
    credentials:
      - access_key_id: "AKID_APP1"
        secret_access_key: "secret1"

  - name: "app2-files"
    credentials:
      - access_key_id: "AKID_APP2_WRITER"
        secret_access_key: "secret2"
      - access_key_id: "AKID_APP2_READER"
        secret_access_key: "secret3"
```

**Generating credentials:** Use `openssl rand` to produce random keys:

```bash
# Generate an access key ID (20 chars, uppercase + digits)
openssl rand -hex 10 | tr '[:lower:]' '[:upper:]'

# Generate a secret access key (40 chars, base64)
openssl rand -base64 30
```

**Validation rules:**
- Bucket names must not contain `/`.
- Bucket names must be unique across the config.
- Access key IDs must be globally unique across all buckets.
- Each bucket must have at least one credential set.
- Each credential needs either `access_key_id` + `secret_access_key` (SigV4) or `token` (legacy).

Multiple credentials on the same bucket let different services share a namespace with independent keys. This is useful when you want a writer service and a reader service accessing the same files.

SigV4 credentials also support presigned URLs automatically. Clients can generate time-limited presigned URLs using any AWS SDK presign client — no additional configuration is needed on the orchestrator side.

