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


## Encrypting objects that already exist

Enabling encryption applies to **new writes only**. Objects already on a backend when you turned it on stay plaintext, and nothing rewrites them on its own. A bucket can sit indefinitely half-encrypted while the config reads as though encryption covers it.

Closing that gap is a one-time, operator-triggered pass:

```bash
s3-orchestrator admin encrypt-existing
```

It walks every copy still recorded as plaintext, downloads it, encrypts it, re-uploads the ciphertext, and updates the ledger row. It is safe to re-run: copies already encrypted are not selected. Expect it to cost a full read and write of every affected object, which is metered egress and ingress on most providers, so run it deliberately rather than on a schedule.

**Knowing whether you need to.** Three places report how much of the fleet is still plaintext:

| Where | What it shows |
|-------|---------------|
| `s3o_encryption_plaintext_copies` | Gauge of copies still stored unencrypted |
| `GET /admin/api/status` | `integrity.plaintext_copies` |
| Web dashboard | **Encryption Coverage** section, shown when encryption is enabled |
| TUI backends pane | `plaintext: N` on the stats line, hidden once the count is zero |

A non-zero value on a fleet configured for encryption means exactly one thing: objects predating the setting were never rewritten. It does not fall on its own.


## Integrity verification


SHA-256 content hashing for data integrity verification. When enabled, objects are checksummed on write and the hash is stored alongside the object location in PostgreSQL.

```yaml
integrity:
  enabled: true
  verify_on_read: true               # Hash-check whole-object GET responses as they stream
  verify_on_replicate: true          # Verify hash when creating replicas (default: true)
  scrubber_interval: "6h"            # Background verification interval (0 = disabled)
  scrubber_batch_size: 100           # Objects per scrub cycle
```

**How it works:**

- **Write path:** SHA-256 is computed on the plaintext body (before encryption) and stored in `object_locations.content_hash`.
- **Read path (`verify_on_read`):** A `VerifyingReader` wraps the response body and computes the hash as data streams to the client. On mismatch at EOF, the corrupted copy is enqueued for cleanup. Range requests are not verified: the stored hash covers the whole object, so a slice of it could never match. The scrubber, which reads whole objects, is what covers data only ever read in ranges.
- **Scrubber:** A background worker works through the copies least recently verified, reads them from their backend, decrypts if needed, and checks the hash. A copy that fails has its bytes discarded and its ledger row removed, so the replicator rebuilds it from a healthy copy. Each read counts against the backend's usage quota.
- **Backfill:** Objects written before integrity was enabled have no stored hash. Use `admin backfill-checksums` to read those objects and compute their hashes.

**Integrity is hot-reloadable** — changes take effect on SIGHUP without a restart.

