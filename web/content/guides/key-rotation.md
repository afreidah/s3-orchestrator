---
title: "Key Rotation"
description: "Rotate the encryption master key on a running instance. DEKs are re-wrapped as a metadata-only operation, so stored objects are never rewritten."
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

Set the new key as `master_key` and move the old key to `previous_keys`, or its file to `previous_key_files`:

```yaml
encryption:
  enabled: true
  master_key: "${NEW_ENCRYPTION_KEY}"
  previous_keys:
    - "${OLD_ENCRYPTION_KEY}"
```

The previous keys allow the orchestrator to decrypt objects that still have DEKs wrapped with the old key. Each key is recorded under its fingerprint, `k-` and 16 hex digits, so a key keeps the same ID wherever it sits in the config.

### Vault Transit

If you're using Vault Transit, rotate the key in Vault itself:

```bash
vault write -f transit/keys/s3-orchestrator/rotate
```

Vault wraps new DEKs with the latest key version on its own, so no config change or restart is needed. To re-wrap the existing DEKs under that version, skip to Step 4 and run it with the Vault key's ID rather than `--all-stale`:

```bash
s3-orchestrator admin rotate-encryption-key --old-key-id vault:transit/s3-orchestrator
```

`--all-stale` selects nothing here: a Vault key's ID is `vault:{mount}/{key}`, which stays the same when Vault rotates the key.

## Step 3: Restart the Orchestrator

Encryption configuration is **not reloadable** - a restart is required.

```bash
systemctl restart s3-orchestrator
```

After restarting, new objects use the new key. Existing objects can still be read because the old key is still configured.

## Step 4: Re-wrap All DEKs

Re-wrap every DEK not already under the new key:

```bash
s3-orchestrator admin rotate-encryption-key --all-stale
```

The command calls `POST /admin/api/rotate-encryption-key` with `{"all_stale": true}`. To re-wrap only one key's DEKs, pass `--old-key-id` (`old_key_id` in the body) with that key's ID:

| Key source | ID format |
|-----------|-----------|
| Inline (`master_key`, `previous_keys`) or file (`master_key_file`, `previous_key_files`) | `k-` and 16 hex digits, the key's fingerprint |
| Vault Transit | `vault:{mount}/{key}` |

Releases before v0.163.0 recorded positional IDs (`config-0`, `config-1`, `file-0`); `--all-stale` moves those rows to the fingerprint as well.

The response shows progress:

```json
{"status": "complete", "rotated": 1423, "failed": 0, "total": 1423}
```

## Step 5: Monitor

Watch these metrics during and after rotation:

| Metric | Description |
|--------|-------------|
| `s3o_key_rotation_objects_total{status="success"}` | DEKs successfully re-wrapped |
| `s3o_key_rotation_objects_total{status="error"}` | DEKs that failed re-wrapping |
| `s3o_key_rotation_objects_total{status="changed"}` | Copies a client overwrote during the pass; the new write is already sealed with the current key |
| `s3o_encryption_unknown_key_id_total` | Decryptions no configured key could unwrap. Any non-zero value after rotation completes indicates objects wrapped with a key that is no longer configured. |

If any DEKs failed, check the logs and retry - the command only processes DEKs not yet under the new key.

## Step 6: Clean Up (Optional)

After all DEKs are re-wrapped, you can remove the old key from the config and restart. Wait first for multipart uploads started before the rotation to complete or be aborted: their parts were sealed under the old key, and the re-wrap pass covers stored objects only.

{{< callout >}}
Keep the old key backed up somewhere safe even after removing it from the config, in case you need to restore from a backup that predates the rotation.
{{< /callout >}}

## Rotation Schedule

There's no built-in automatic rotation. Establish a rotation cadence based on your security requirements - quarterly or annually is common. The operation is fast since it only updates metadata, so it can be done during normal operations without downtime.
