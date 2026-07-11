---
title: "Encryption"
linkTitle: "Encryption"
weight: 27
---

# Encryption


Server-side envelope encryption with chunked AES-256-GCM. When enabled, objects are encrypted before being stored on backends and decrypted transparently on read. Exactly one key source is required.

```yaml
encryption:
  enabled: true
  chunk_size: 65536                    # default: 64KB (range: 4KB–1MB, must be power of 2)
  master_key: "${ENCRYPTION_KEY}"      # base64-encoded 256-bit key
```

**Generating a master key:**

```bash
openssl rand -base64 32
```

**Key source options** — exactly one of the following must be set:

| Source | Config field | When to use |
|--------|-------------|-------------|
| Inline | `master_key` | Base64-encoded 256-bit key in config or env var. Simplest option. |
| File | `master_key_file` | Path to a file containing exactly 32 raw bytes. Good for bare-metal with config management. |
| Vault Transit | `vault` | Delegate key wrapping/unwrapping to HashiCorp Vault. Best for production with HSM-backed key management. |

**Vault Transit configuration:**

```yaml
encryption:
  enabled: true
  vault:
    address: "http://vault.service.consul:8200"
    token: "${VAULT_TOKEN}"
    key_name: "s3-orchestrator"
    mount_path: "transit"     # default: transit
```

The Vault Transit engine handles wrapping and unwrapping DEKs — the orchestrator never sees the master key material. The `key_name` must reference an existing key in the Transit engine.

**Key rotation support:**

When rotating to a new master key, move the old key to `previous_keys` so existing objects can still be decrypted:

```yaml
encryption:
  enabled: true
  master_key: "${NEW_ENCRYPTION_KEY}"         # new primary key
  previous_keys:
    - "${OLD_ENCRYPTION_KEY}"                 # old key, kept for unwrapping
```

After updating the config, call the `rotate-encryption-key` admin API to re-wrap all DEKs with the new key. See [Rotating encryption keys](#rotating-encryption-keys) below.

**Important notes:**
- Per-object data-encryption keys and wrapped key material are held server-side only. They are never serialized in API responses - the admin object-locations endpoint reports the `encrypted` flag and `key_id`, never the key itself.
- Encryption is **not reloadable** — changing encryption settings requires a restart.
- The `chunk_size` must stay the same for the lifetime of the data. Changing it after objects are encrypted will make those objects unreadable.
- Encrypted objects are slightly larger than their plaintext (header + per-chunk overhead). The exact overhead is: 32 bytes (header) + 28 bytes per chunk (nonce + auth tag).


## Integrity verification


SHA-256 content hashing for data integrity verification. When enabled, objects are checksummed on write and the hash is stored alongside the object location in PostgreSQL.

```yaml
integrity:
  enabled: true
  verify_on_read: true               # Hash-check every GET response as it streams
  verify_on_replicate: true          # Verify hash when creating replicas (default: true)
  scrubber_interval: "6h"            # Background verification interval (0 = disabled)
  scrubber_batch_size: 100           # Objects per scrub cycle
```

**How it works:**

- **Write path:** SHA-256 is computed on the plaintext body (before encryption) and stored in `object_locations.content_hash`.
- **Read path (`verify_on_read`):** A `VerifyingReader` wraps the response body and computes the hash as data streams to the client. On mismatch at EOF, the corrupted copy is enqueued for cleanup.
- **Scrubber:** A background worker periodically reads random objects from backends, decrypts if needed, and verifies their hash. Corrupted copies are enqueued for cleanup. Each read counts against the backend's usage quota.
- **Backfill:** Objects written before integrity was enabled have no stored hash. Use `admin backfill-checksums` to read those objects and compute their hashes.

**Integrity is hot-reloadable** — changes take effect on SIGHUP without a restart.

