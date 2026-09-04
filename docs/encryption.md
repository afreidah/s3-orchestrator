---
description: "Server-side envelope encryption with chunked AES-256-GCM, the key sources it supports, and how existing objects are migrated to it."
title: "Encryption"
linkTitle: "Encryption"
weight: 27
---


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
- Encrypted objects are slightly larger than their plaintext (header + per-chunk overhead). The exact overhead is: 32 bytes (header) + 28 bytes per chunk (nonce + auth tag). Because that overhead is a fixed function of the size, the stored size of a write is known before it starts, and placement and the usage counters both use it rather than the size the client announced.


## Composition with compression

When [compression](compression.md) is also enabled, it runs first, in that order only: ciphertext does not compress. The compressed stream is therefore what the encryptor takes as its input, which is what `plaintext_size` records - the pre-encryption size, not the object the client wrote. That size lives in `logical_size` instead.

A read runs the layers backwards: decrypt, then decompress, then slice. Ranged reads translate twice, from a logical range to the frames covering it, and from those to the ciphertext chunks holding them. Both translations use the same chunk arithmetic, because the compressed stream is the encryptor's plaintext domain.

An object that is both compressed and encrypted cannot be recognised from its bytes alone, since the encoding is inside the ciphertext. Reconcile therefore takes its description from a surviving copy's row rather than inspecting the object.


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
  verify_on_replicate: false         # Read each new replica back and hash-check it
  scrubber_interval: "6h"            # Background verification interval (0 = disabled)
  scrubber_batch_size: 100           # Objects per scrub cycle
```

**How it works:**

- **Write path:** SHA-256 is computed on the plaintext body (before encryption) and stored in `object_locations.content_hash`.
- **Read path (`verify_on_read`):** A `VerifyingReader` wraps the response body and computes the hash as data streams to the client. On mismatch at EOF, the corrupted copy is enqueued for cleanup. Range requests are not verified: the stored hash covers the whole object, so a slice of it could never match. The scrubber, which reads whole objects, is what covers data only ever read in ranges.
- **Replication (`verify_on_replicate`):** Each new copy is read back from its target, decrypted and decompressed, and checked against the source's hash before it is recorded. Off by default: the read-back doubles the egress a replica costs. See [configuration.md#integrity](../configuration/#integrity) for what a mismatch means and which failures leave the copy in place.
- **Scrubber:** A background worker works through the copies least recently verified, reads them from their backend, decrypts if needed, and checks the hash. A copy that fails has its bytes discarded and its ledger row removed, so the replicator rebuilds it from a healthy copy. Each read counts against the backend's usage quota.
- **Backfill:** Objects written before integrity was enabled have no stored hash. Use `admin backfill-checksums` to read those objects and compute their hashes.

**Integrity is hot-reloadable** — changes take effect on SIGHUP without a restart.

