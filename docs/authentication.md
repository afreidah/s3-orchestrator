---
description: "How a credential resolves to a user, how that user's grants decide which virtual buckets it reaches, and how every request is authenticated and authorised before reaching a backend."
title: "Authentication and Credentials"
linkTitle: "Authentication"
weight: 22
---

## SigV4 and multi-bucket auth


A credential proves a caller is one **user**. That user holds a **grant** on each resource it may reach - a virtual bucket for object access, a backend or the instance itself for the control plane. On every request, the orchestrator:

1. Extracts the access key from the SigV4 `Authorization` header or the presigned URL query parameters.
2. Resolves the credential to the user behind it.
3. Verifies the signature.
4. Asks that user whether it holds a grant on the bucket in the URL path.

There is one credential type, an access key and secret, presented two ways:

1. **AWS SigV4** - Standard AWS Signature Version 4 via the `Authorization` header. Compatible with `aws cli`, SDKs, and any S3 client. Signature verification is constant-time: unknown access keys still compute a full HMAC to prevent timing side-channel enumeration. Streaming-payload uploads (`STREAMING-AWS4-HMAC-SHA256-PAYLOAD`, `STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER`, `STREAMING-UNSIGNED-PAYLOAD-TRAILER`) are accepted and the chunk chain is fully validated end-to-end.
2. **Presigned URLs** - SigV4 query-parameter authentication (`X-Amz-Algorithm`, `X-Amz-Credential`, etc.) for time-limited, shareable URLs. Works with any AWS SDK presign client. Maximum expiry: 7 days. Uses the same credentials as normal requests - no additional configuration required.

Access key IDs must be globally unique. Authentication is always required, and a user holding no grants authenticates successfully and is refused on every bucket - which is a clearer answer for a client that has been created but not yet granted anything than a failed signature would be.

For client usage examples (AWS CLI, rclone, boto3, Go SDK), see the [User Guide](user-guide.md).
For credential rotation procedures, see [docs/operations.md](operations.md#rotating-client-credentials).

## Where credentials come from

A deployment declares credentials in two places, and both are live at once.

**The config file** declares them per bucket, as below. A credential declared this way is merged in as a user reaching exactly the one bucket that declared it, so it resolves through the same user-and-grant chain as any other and the request path has no second case to handle.

**The store** holds users, their credentials, and their grants as rows, created through the provisioning API or the `bucket`, `user`, `credential` and `grant` CLI commands. A stored credential can reach several buckets, because its user can hold several grants, and a bucket can be granted to several users.

Config wins a collision, and the API refuses to modify anything the config file declares. See [config versus the provisioning API](configuration.md#config-versus-the-provisioning-api) for the precedence rule, and [the CLI reference](cli.md#bucket-user-credential-and-grant) for the commands.

## The admin surface

The admin API takes the same credential the S3 API does: an access key and secret, SigV4-signed, resolving to the user that holds the grants. One keypair reaches both surfaces, and the endpoints under `/admin/api/objects` are authorized against the same grants and the same permissions as the S3 path.

The identity that administers a deployment is declared under `auth.root`:

```yaml
auth:
  root:
    access_key_id: "AKIAROOT..."
    secret_access_key: "..."
```

It is an ordinary user that happens to hold every permission on every resource. There is no separate code path authorizing it, which is what makes it possible to issue narrower administrative credentials and have them behave predictably.

Declaring it is optional. A deployment that administers itself through credentials its store already holds can leave `auth.root` out and run as a pure S3 endpoint. Enabling the dashboard requires it, because a deployment with no credential at all would have nothing able to log in and create the first user.

The rest of the admin API is the control plane - backend drain, key rotation, provisioning, worker triggers. Each of those endpoints declares a permission over a **backend** or over the **instance**, and a credential reaches it only by holding a grant carrying that permission. A credential holding only bucket grants is refused on all of them.

A pass that names no backend runs against every one, so it is authorized as `backend:*`: an operator granted one provider cannot start a conversion that spends egress on the rest of the fleet. See [the admin API's authorization section](admin-api.md#authorization) for the permission each endpoint declares.

The root credential reaches everything because the root user holds everything, not because the credential is special.

The dashboard logs in with a credential too, and its session carries the user that credential proved. One keypair therefore reaches the data, the admin API and the dashboard, and what it can do in each is decided by the same grants.

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
      - access_key_id: "AKID_APP2_INGEST"
        secret_access_key: "secret2"
      - access_key_id: "AKID_APP2_ANALYTICS"
        secret_access_key: "secret3"
```

**Generating credentials:** Use `openssl rand` to produce random keys:

```bash
# Generate an access key ID (20 chars, uppercase + digits)
openssl rand -hex 10 | tr '[:lower:]' '[:upper:]'

# Generate a secret access key (40 chars, base64)
openssl rand -base64 30
```

The provisioning API mints both halves for you, so a stored credential needs neither command:

```bash
s3-orchestrator admin credential issue -user <user_id> -label "ingest job"
```

**Validation rules:**
- Bucket names must not contain `/`.
- Bucket names must be unique across the config.
- Access key IDs must be globally unique across all buckets. A key proves one user, so a shared one has no unambiguous identity and startup fails.
- Each bucket must have at least one credential set.
- Each credential needs both halves: `access_key_id` and `secret_access_key`.

### Several credentials on one bucket

Every credential the config file declares on a bucket has identical access to it: the two above can both list, upload, overwrite and delete everything under `app2-files`. The config file has no syntax for narrowing that, so a read-only client is provisioned through the store instead - `grant add -permissions list-buckets,list,read` - and reaches the same bucket with less.

What separate credentials buy on one bucket is worth having either way. Each rotates and is revoked on its own, so a leaked key is withdrawn without interrupting the other services sharing the bucket. Each resolves to its own user, so the audit trail attributes an action to the service that took it rather than to a key shared by several. And revoking one leaves its siblings working, which is what makes zero-downtime rotation possible.

Scoping a grant below the whole bucket - reaching one prefix of it rather than all of it - is tracked in [#1495](https://github.com/afreidah/s3-orchestrator/issues/1495).

SigV4 credentials also support presigned URLs automatically. Clients can generate time-limited presigned URLs using any AWS SDK presign client - no additional configuration is needed on the orchestrator side.
