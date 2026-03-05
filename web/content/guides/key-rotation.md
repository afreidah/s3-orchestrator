---
title: "Key Rotation"
weight: 2
---


This guide walks through rotating the encryption master key on a running S3 Orchestrator. Key rotation re-wraps data encryption keys (DEKs) with a new master key - it's a metadata-only operation and is fast regardless of object sizes.

## Overview

S3 Orchestrator uses envelope encryption: each object has its own DEK, and the DEK is encrypted (wrapped) with the master key. Key rotation replaces the master key used to wrap DEKs without touching the object data itself.

## Step 1: Generate a New Master Key

```bash
openssl rand -base64 32
```

## Step 2: Update the Config

Set the new key as `master_key` and move the old key to `previous_keys`:

```yaml
encryption:
  enabled: true
  master_key: "${NEW_ENCRYPTION_KEY}"
  previous_keys:
    - "${OLD_ENCRYPTION_KEY}"
```

The `previous_keys` list allows the orchestrator to decrypt objects that still have DEKs wrapped with the old key.

### Vault Transit

If you're using Vault Transit, rotate the key in Vault itself:

```bash
vault write -f transit/keys/s3-orchestrator/rotate
```

The orchestrator uses the latest key version automatically. No config change or restart is needed for Vault-managed rotation - skip to Step 4.

## Step 3: Restart the Orchestrator

Encryption configuration is **not reloadable** - a restart is required.

```bash
systemctl restart s3-orchestrator
```

After restarting, new objects use the new key. Existing objects can still be read because the old key is in `previous_keys`.

## Step 4: Re-wrap All DEKs

Run the admin command to re-wrap all DEKs with the new master key:

```bash
s3-orchestrator admin rotate-encryption-key --old-key-id config-0
```

The `--old-key-id` identifies which key's DEKs to re-wrap:

| Key source | ID format |
|-----------|-----------|
| Inline (`master_key`) | `config-0` (primary), `config-1`, `config-2`, ... (previous keys in order) |
| File (`master_key_file`) | `file-0` |
| Vault Transit | The key name (e.g., `s3-orchestrator`) |

The response shows progress:

```json
{"status": "complete", "rotated": 1423, "failed": 0, "total": 1423}
```

## Step 5: Monitor

Watch these metrics during and after rotation:

| Metric | Description |
|--------|-------------|
| `s3proxy_key_rotation_objects_total{status="success"}` | DEKs successfully re-wrapped |
| `s3proxy_key_rotation_objects_total{status="error"}` | DEKs that failed re-wrapping |

If any DEKs failed, check the logs and retry - the command only processes DEKs still wrapped with the old key.

## Step 6: Clean Up (Optional)

After all DEKs are re-wrapped, you can remove the old key from `previous_keys` and restart. Objects that were rotated no longer need the old key.

{{% notice tip %}}
Keep the old key backed up somewhere safe even after removing it from the config, in case you need to restore from a backup that predates the rotation.
{{% /notice %}}

## Rotation Schedule

There's no built-in automatic rotation. Establish a rotation cadence based on your security requirements - quarterly or annually is common. The operation is fast since it only updates metadata, so it can be done during normal operations without downtime.
