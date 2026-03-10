---
title: "Encrypting Existing Data"
weight: 2
---


This guide walks through enabling server-side encryption on an S3 Orchestrator instance that already has unencrypted objects stored across its backends.

## Overview

When you enable encryption, only **new** objects are encrypted automatically. Existing objects remain unencrypted until you explicitly encrypt them using the `encrypt-existing` admin command. This is a one-time operation that processes all unencrypted objects in batches.

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

You can also use a file-based key or Vault Transit. See the [admin guide](../../docs/admin-guide/#encryption) for all key source options.

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

Run the admin command to encrypt all existing unencrypted objects:

```bash
s3-orchestrator admin encrypt-existing
```

This processes objects in batches of 100: each object is downloaded from its backend, encrypted, re-uploaded (overwriting the plaintext), and its database record is updated. The response shows progress:

```json
{"status": "complete", "encrypted": 1423, "failed": 0, "total": 1423}
```

{{% notice warning %}}
This operation downloads and re-uploads every unencrypted object. The re-uploads count against backend quotas and usage limits (API requests, egress, ingress). Plan for the additional network traffic and time, especially with large datasets.
{{% /notice %}}

## Step 5: Verify

Monitor the `s3proxy_encrypt_existing_objects_total` metric:

| Label | Meaning |
|-------|---------|
| `success` | Objects successfully encrypted |
| `error` | Objects that failed encryption |

If any objects failed, check the logs for details and run `encrypt-existing` again - it only processes objects without encryption metadata, so it's safe to retry.

## Important Notes

- The `chunk_size` (default: 64KB) must stay the same for the lifetime of the data. Changing it after objects are encrypted makes those objects unreadable.
- Encrypted objects are slightly larger: 32 bytes (header) + 28 bytes per chunk (nonce + auth tag).
- The operation is idempotent - running it multiple times only processes unencrypted objects.
