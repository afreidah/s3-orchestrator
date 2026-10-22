---
title: "Encrypting Existing Data"
description: "Enable server-side encryption on an instance that already holds unencrypted objects, and migrate the existing data across its backends."
weight: 2
---


This guide walks through enabling server-side encryption on an S3 Orchestrator instance that already has unencrypted objects stored across its backends.

## Overview

When you enable encryption, only **new** objects are encrypted automatically. Existing objects remain unencrypted until you explicitly encrypt them via the `/admin/api/encrypt-existing` endpoint. This is a one-time operation that processes all unencrypted objects in batches.

## Prerequisites

- A running S3 Orchestrator instance with existing unencrypted data
- Admin API access (via CLI or direct HTTP)
- A 256-bit encryption key (or Vault Transit configured)

## Step 1: Generate a Master Key

```bash
openssl rand -base64 32
```

Save this key securely - you'll need it for the config and for any future key rotation.

## Step 2: Enable Encryption in Config

Add the `encryption` section to your config file:

```yaml
encryption:
  enabled: true
  master_key: "${ENCRYPTION_KEY}"
```

You can also use a file-based key or Vault Transit. See the [admin guide](../../docs/encryption/) for all key source options.

### Vault Transit (recommended for production)

```yaml
encryption:
  enabled: true
  vault:
    address: "http://vault.service.consul:8200"
    token: "${VAULT_TOKEN}"
    key_name: "s3-orchestrator"
    mount_path: "transit"
```

## Step 3: Restart the Orchestrator

Encryption configuration is **not reloadable** - a restart is required.

```bash
systemctl restart s3-orchestrator
```

After restarting, all new objects will be encrypted automatically. Existing objects are still unencrypted at this point.

## Step 4: Encrypt Existing Objects

Encrypt all existing unencrypted objects with the CLI:

```bash
s3-orchestrator admin encrypt-existing
```

The CLI signs the request with the keypair from `-access-key`/`-secret-key` or `$S3O_ACCESS_KEY_ID`/`$S3O_SECRET_ACCESS_KEY`: the `auth.root` credential, or one granted `admin.convert`. Pass `-backend NAME` to convert one backend's copies, or `-max N` to stop after N copies.

This processes copies in batches of 100: each copy is downloaded from its backend, encrypted, and written to a new path on the same backend. The new row replaces the plaintext one in one transaction, then the plaintext bytes are deleted, so a reader sees either the old copy or the new one. A compressed copy keeps its encoding. The response shows progress:

```json
{"status": "complete", "encrypted": 1423, "skipped": 0, "changed": 0, "failed": 0, "total": 1423}
```

`skipped` counts copies on a backend with no usage headroom, no room for the larger ciphertext, or a drain in progress. `changed` counts copies a client wrote while the pass was converting them: the client's write stands and the pass deletes the bytes it wrote. See [When a conversion meets a live write](../../docs/admin-api/#when-a-conversion-meets-a-live-write).

{{< callout type="warning" >}}
This operation downloads and rewrites every unencrypted copy. Each rewrite costs a GET, a PUT and a DELETE and counts against backend quotas and usage limits (API requests, egress, ingress). Plan for the additional network traffic and time, especially with large datasets.
{{< /callout >}}

## Step 5: Verify

Monitor the `s3o_encrypt_existing_objects_total` metric:

| Label | Meaning |
|-------|---------|
| `success` | Copies successfully encrypted |
| `skipped` | Copies the pass left alone this time |
| `changed` | Copies a client wrote during the pass; the client's write stands |
| `error` | Copies that failed encryption |

If any objects failed, check the logs for details and re-POST to `/admin/api/encrypt-existing` again - it only processes objects without encryption metadata, so it's safe to retry.

## Important Notes

- The `chunk_size` (default: 64KB) must stay the same for the lifetime of the data. Changing it after objects are encrypted makes those objects unreadable.
- Encrypted objects are slightly larger: 32 bytes (header) + 28 bytes per chunk (nonce + auth tag).
- The operation is idempotent - running it multiple times only processes unencrypted objects.
