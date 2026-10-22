---
description: "Interactive diagram of the envelope encryption and decryption paths, using chunked AES-256-GCM with pluggable key providers."
title: "Encryption Flow"
linkTitle: "Encryption Flow"
weight: 5
---

Envelope encryption and decryption paths for S3 objects using chunked AES-256-GCM with pluggable key providers. **Hover over any component** in the diagram for implementation details.

---

### How it works

When encryption is enabled, every object stored through the S3 Orchestrator is encrypted before it leaves the server. The system uses **envelope encryption** - a two-layer key scheme where each object gets its own throwaway key, and that key is itself encrypted by a master key. Encryption is optional - see the [encryption guide](../../docs/encryption/) for how to enable it.

#### Key concepts

- **DEK (Data Encryption Key)** - A random 32-byte (256-bit) key generated fresh for every object. This is the key that actually encrypts the data. No two objects share a DEK, so compromising one object's key reveals nothing about any other object.

- **Master key** - A long-lived key managed by a key provider (Vault Transit, a config value, or a key file). The master key never touches the data directly - it only encrypts and decrypts DEKs. This means the master key can be rotated without re-encrypting every object.

- **Nonce** - A 12-byte random value that ensures the same plaintext encrypted with the same key produces different ciphertext each time. The system generates one **base nonce** per object, then mathematically derives a unique nonce for each chunk by XORing the base nonce with the chunk's index number.

- **AES-256-GCM** - The encryption algorithm. GCM mode provides both confidentiality (data is unreadable) and authenticity (any tampering is detected). Each chunk produces a 16-byte authentication tag that acts as a tamper seal.

#### Encrypting an object (write path)

1. **Generate a DEK**: 32 random bytes from the OS cryptographic random source.
2. **Wrap the DEK**: The master key encrypts the DEK via the configured key provider (e.g., Vault Transit API call). This produces a **wrapped DEK** - an opaque blob that can only be unwrapped by the same master key.
3. **Generate a base nonce**: 12 random bytes.
4. **Write the header**: A 32-byte header is prepended to the ciphertext stream - `"SENC"` magic bytes, format version, chunk size, and the base nonce.
5. **Encrypt chunk by chunk** (default 64 KiB per chunk):
   - Read up to one chunk of plaintext.
   - Derive this chunk's nonce: take the base nonce and XOR the chunk index into its last 8 bytes.
   - Encrypt the chunk with AES-256-GCM using the DEK and the derived nonce. This produces ciphertext + a 16-byte auth tag.
   - Write to the output: `nonce (12 bytes) | ciphertext | auth tag (16 bytes)`.
   - Repeat until all plaintext is consumed.
6. **Upload the ciphertext** (header + chunks) to the S3 backend. The ciphertext is produced once per write and buffered (in memory up to 32 MiB, in a temp file above that), so every upload attempt and every copy sends identical bytes.
7. **Store metadata in the metadata store**: the wrapped DEK, the master key ID, the base nonce (packed together as `baseNonce || wrappedDEK` in the `encryption_key` column), and the plaintext size.

The plaintext DEK is never stored anywhere - it exists only in memory during the encryption operation.

"Plaintext" here means whatever the encryptor was handed, which is not always the object the client wrote. With [compression](../../docs/compression/) also enabled, the object is encoded first - ciphertext does not compress - so the encryptor's input is the compressed stream and `plaintext_size` records that. The client's own size lives in `logical_size`. When an object is stored unencoded - compression off, or the object skipped by `min_size` or `min_ratio` - the two are the same and `logical_size` is unset.

#### Decrypting an object (read path)

1. **Retrieve metadata from the metadata store**: the wrapped DEK, key ID, and base nonce.
2. **Unwrap the DEK**: The key provider decrypts the wrapped DEK using the master key identified by the stored key ID, recovering the original 32-byte DEK.
3. **Parse the header**: Read the 32-byte header from the ciphertext stream to get the chunk size and base nonce.
4. **Decrypt chunk by chunk**:
   - Read one chunk: `nonce (12B) | ciphertext | auth tag (16B)`.
   - Verify the nonce matches the expected value (base nonce XOR chunk index) - this detects reordering or insertion attacks.
   - Decrypt with AES-256-GCM, which also verifies the auth tag - if the data was tampered with, decryption fails.
   - Stream the plaintext to the client.

#### Range requests (partial reads)

When a client requests a byte range (e.g., `Range: bytes=1000-2000`), the system doesn't need to download and decrypt the entire object:

1. **Translate the plaintext byte range to ciphertext byte range**: figure out which chunks contain the requested bytes, accounting for the 32-byte header and 28-byte overhead per chunk.
2. **Fetch only those chunks** from the S3 backend using a range request.
3. **Decrypt just the fetched chunks** (typically one or two chunks for a small range).
4. **Slice the plaintext** to the exact requested bytes and stream them to the client.

The base nonce is stored in the database specifically so range decryption can derive per-chunk nonces without fetching the ciphertext header from the backend.

---

{{< diagram >}}

<script>
window.s3oDiagram = {
  source: [
    'flowchart TD',
    '    PLAIN([Plaintext<br>Stream]):::entry --> DEK[Generate Random<br>256-bit DEK]:::process',
    '    DEK --> WRAP[WrapDEK via<br>KeyProvider]:::storage',
    '    WRAP --> PROVIDER{Key Provider<br>Type}:::decision',
    '    PROVIDER -->|Vault Transit| VAULT[Vault Transit<br>Encrypt API]:::storage',
    '    PROVIDER -->|Config / File| LOCAL[AES-GCM Local<br>Key Wrap]:::process',
    '    VAULT --> HDR',
    '    LOCAL --> HDR',
    '',
    '    HDR[Write 32B Header<br>SENC + Nonce]:::process --> CHUNK[Read Plaintext<br>Chunk]:::process',
    '    CHUNK --> NONCE[Derive Chunk Nonce<br>Base XOR Index]:::process',
    '    NONCE --> SEAL[AES-256-GCM<br>Seal Chunk]:::process',
    '    SEAL --> MORE{More<br>Chunks?}:::decision',
    '    MORE -->|yes| CHUNK',
    '    MORE -->|no| CTOUT([Ciphertext +<br>Metadata]):::success',
    '',
    '    CTIN([Ciphertext<br>Full Object]):::entry --> UNWRAP[UnwrapDEK via<br>KeyProvider]:::storage',
    '    UNWRAP --> PARSE[Parse 32B Header<br>Extract Nonce]:::process',
    '    PARSE --> DCHUNK[Read Ciphertext<br>Chunk]:::process',
    '    DCHUNK --> VERIFY[Verify Nonce<br>Match Index]:::filter',
    '    VERIFY --> OPEN[AES-256-GCM<br>Open Chunk]:::process',
    '    OPEN --> DMORE{More<br>Chunks?}:::decision',
    '    DMORE -->|yes| DCHUNK',
    '    DMORE -->|no| PTOUT([Plaintext<br>Stream]):::success',
    '',
    '    RANGE([Range Request<br>start-end]):::entry --> XLATE[CiphertextRange<br>Translation]:::process',
    '    XLATE --> FETCH[Fetch Ciphertext<br>Chunks Only]:::storage',
    '    FETCH --> RUNWRAP[UnwrapDEK via<br>KeyProvider]:::storage',
    '    RUNWRAP --> RDEC[Decrypt Relevant<br>Chunks]:::process',
    '    RDEC --> SLICE[Skip + Limit<br>to Byte Range]:::filter',
    '    SLICE --> ROUT([Range Plaintext<br>Bytes]):::success',
    '',
  ].join('\n'),

  info: {
    PLAIN: {
      title: 'Plaintext Stream',
      badge: 'entry', badgeText: 'encrypt entry',
      body: '<p>Plaintext body from <code>PutObject</code>, already buffered by the write path. The MD5 for the client-facing ETag (and the optional SHA-256 content hash) were computed over the client\'s bytes during that buffering pass, before compression or encryption.</p><p>The <code>Encryptor.Encrypt(ctx, body, plaintextSize)</code> method runs the envelope encryption pipeline. It does not compute the ETag.</p>'
    },
    DEK: {
      title: 'Generate Random 256-bit DEK',
      badge: 'process', badgeText: 'key generation',
      body: '<p>Generates a 32-byte (256-bit) Data Encryption Key using <code>crypto/rand.Read(dek)</code>. Each object gets a unique DEK &mdash; no key reuse across objects.</p><p>A <code>PutObject</code> encrypts once, before the failover loop. Every attempt and every copy sends the same ciphertext.</p><p>Multipart uploads wrap one DEK at <code>CreateMultipartUpload</code> and reuse it for every part through <code>EncryptWithDEK</code>, which draws a fresh base nonce per part.</p>'
    },
    WRAP: {
      title: 'WrapDEK via KeyProvider',
      badge: 'storage', badgeText: 'key wrapping',
      body: '<p><code>provider.WrapDEK(ctx, dek)</code> encrypts the plaintext DEK with the master key. Returns <code>(wrappedDEK, keyID, error)</code>.</p><p>The <code>keyID</code> identifies which master key was used, enabling key rotation via <code>MultiKeyProvider</code>. The wrapped DEK and keyID are stored in the metadata store alongside the object record.</p><p class="ac-metric">Metric: s3o_encryption_operations_total{op="encrypt"}, counted once per completed encrypt after <code>Encrypt</code> returns, not per wrap call</p>'
    },
    PROVIDER: {
      title: 'Key Provider Type',
      badge: 'decision', badgeText: 'provider selection',
      body: '<p>Resolved at startup by <code>NewKeyProviderFromConfig()</code>. Two provider types:</p><p><b>Vault Transit</b>: master key never leaves Vault; DEK wrapped via HTTP API, ID <code>vault:{mount}/{key}</code><br><b>StaticKeyProvider</b>: a 256-bit key held in memory, loaded inline from <code>master_key</code> (base64) or from <code>master_key_file</code> (32 raw bytes). Its ID is the key\'s fingerprint, <code>k-</code> and the first 8 bytes of HMAC-SHA256 keyed by the key, so a key keeps its ID wherever it sits in the config.</p><p><code>MultiKeyProvider</code> wraps any primary with the old keys from <code>previous_keys</code> and <code>previous_key_files</code> for rotation: <code>WrapDEK</code> always uses primary; <code>UnwrapDEK</code> resolves by <code>keyID</code>.</p>'
    },
    VAULT: {
      title: 'Vault Transit Encrypt API',
      badge: 'storage', badgeText: 'external KMS',
      body: '<p>PUT (a Vault write) to <code>{vault}/v1/{mount}/encrypt/{keyName}</code> with base64-encoded DEK as plaintext payload. <code>X-Vault-Token</code> header for auth.</p><p>Returns a Vault ciphertext blob (e.g., <code>vault:v1:...</code>) stored as the wrapped DEK. The master key <b>never leaves Vault</b>.</p><p>KeyID format: <code>vault:{mountPath}/{keyName}</code>. HTTP client has 10s timeout and optional custom CA cert for TLS.</p>'
    },
    LOCAL: {
      title: 'AES-GCM Local Key Wrap',
      badge: 'process', badgeText: 'local wrapping',
      body: '<p><code>aesGCMWrap(masterKey, dek)</code>: creates AES-256-GCM cipher from the 32-byte master key, generates a random 12-byte nonce, and seals the DEK.</p><p>Output format: <code>nonce (12B) || ciphertext+tag</code>. The <code>aesGCMUnwrap</code> reversal splits on the nonce boundary.</p><p>Used by <code>StaticKeyProvider</code>, which holds the keys from config, env and key files.</p>'
    },
    HDR: {
      title: 'Write 32-Byte Header',
      badge: 'process', badgeText: 'wire format',
      body: '<p>The <code>encryptReader</code> emits a 32-byte header before any ciphertext chunks:</p><p><code>SENC</code> magic (4B) + version <code>0x01</code> (1B) + chunk_size big-endian (4B) + baseNonce (12B) + reserved zeros (11B)</p><p>The base nonce is also stored in the DB via <code>PackKeyData(baseNonce || wrappedDEK)</code> for range decryption without re-fetching the header from the backend.</p>'
    },
    CHUNK: {
      title: 'Read Plaintext Chunk',
      badge: 'process', badgeText: 'chunking',
      body: '<p><code>io.ReadFull(src, plain[:chunkSize])</code> reads up to <code>chunkSize</code> bytes (default 65536 bytes; configurable 4 KiB-1 MiB, power of two). The last chunk may be shorter.</p><p>Streaming design: <code>encryptReader</code> implements <code>io.Reader</code>, encrypting one chunk per <code>Read()</code> call. <code>PutObject</code> materializes its output once (in memory up to 32 MiB, in a temp file above that) so every upload attempt replays the same bytes.</p>'
    },
    NONCE: {
      title: 'Derive Per-Chunk Nonce',
      badge: 'process', badgeText: 'nonce derivation',
      body: '<p><code>chunkNonce(base, idx)</code>: copies the 12-byte base nonce, then XORs the chunk index (as big-endian uint64) into the last 8 bytes.</p><p>This gives each chunk a unique nonce deterministically derived from the base, enabling random-access decryption by chunk index without storing per-chunk nonces separately.</p><p>The nonce is prepended to each ciphertext chunk in the wire format.</p>'
    },
    SEAL: {
      title: 'AES-256-GCM Seal Chunk',
      badge: 'process', badgeText: 'encryption',
      body: '<p><code>gcm.Seal(nil, nonce, plain, nil)</code> encrypts the plaintext chunk and appends the 16-byte authentication tag.</p><p>Per-chunk wire format: <code>nonce (12B) + ciphertext (up to chunkSize B) + tag (16B)</code>. Total overhead per chunk: 28 bytes (<code>ChunkOverhead = NonceSize + TagSize</code>).</p><p>Ciphertext size formula: <code>HeaderSize + fullChunks * (ChunkOverhead + chunkSize) + [ChunkOverhead + remainder]</code></p>'
    },
    MORE: {
      title: 'More Chunks?',
      badge: 'decision', badgeText: 'loop control',
      body: '<p>The <code>encryptReader.Read()</code> loop continues until <code>io.ReadFull</code> returns <code>io.EOF</code> or <code>io.ErrUnexpectedEOF</code> (last partial chunk), at which point <code>srcDone = true</code>.</p>'
    },
    CTOUT: {
      title: 'Ciphertext + Metadata',
      badge: 'success', badgeText: 'encrypt output',
      body: '<p><code>EncryptResult</code> contains everything needed for storage:</p><p><b>Body</b>: streaming ciphertext reader (header + chunks)<br><b>CiphertextSize</b>: pre-computed via <code>ciphertextSizeExact()</code><br><b>WrappedDEK</b>: encrypted DEK bytes<br><b>KeyID</b>: master key identifier<br><b>BaseNonce</b>: 12-byte nonce for range decryption</p><p>DB stores <code>PackKeyData(baseNonce || wrappedDEK)</code> and <code>keyID</code> in the object record.</p>'
    },
    CTIN: {
      title: 'Ciphertext (Full Object)',
      badge: 'entry', badgeText: 'decrypt entry',
      body: '<p>Full ciphertext stream fetched from the S3 backend via <code>GetObject</code>. The <code>Encryptor.Decrypt(ctx, body, wrappedDEK, keyID)</code> method handles the complete decryption flow.</p><p>The wrapped DEK and keyID are retrieved from the metadata store along with the object location metadata.</p><p>Before decrypting a read that starts at byte 0, the GET path peeks at the first bytes for the envelope signature. If the row\'s <code>encrypted</code> flag disagrees with what is stored, that copy is skipped and the read fails over to the next one, rather than serving ciphertext or failing to decrypt plaintext.</p><p class="ac-metric">Metric: s3o_encryption_flag_mismatch_total{op="get"}</p>'
    },
    UNWRAP: {
      title: 'UnwrapDEK via KeyProvider',
      badge: 'storage', badgeText: 'key unwrapping',
      body: '<p><code>provider.UnwrapDEK(ctx, wrappedDEK, keyID)</code> recovers the plaintext 32-byte DEK.</p><p>For <code>MultiKeyProvider</code>: if <code>keyID</code> matches the primary key, uses primary; otherwise looks up in the <code>previous</code> map by keyID. An ID no configured key carries, such as the positional <code>config-0</code> and <code>file-0</code> releases before v0.163.0 recorded, is tried against every key in turn; GCM authentication rejects each key that did not wrap the DEK. When none unwraps it, the counter below increments, an error is logged, and the object stays unreadable until its key is added to <code>previous_keys</code> or <code>previous_key_files</code>.</p><p>Vault Transit: PUT (a Vault write) to <code>{vault}/v1/{mount}/decrypt/{keyName}</code>, returns base64-decoded plaintext DEK.</p><p class="ac-metric">Metrics: s3o_encryption_unknown_key_id_total; s3o_encryption_operations_total{op="decrypt"} or {op="decrypt_range"}, counted in <code>DecryptStored</code> once per completed decrypt setup, not per unwrap call</p>'
    },
    PARSE: {
      title: 'Parse 32-Byte Header',
      badge: 'process', badgeText: 'header parsing',
      body: '<p><code>ParseHeader(r)</code> reads 32 bytes, validates the <code>SENC</code> magic and version <code>0x01</code>, extracts chunk size (big-endian uint32 at offset 5) and base nonce (12 bytes at offset 9).</p><p>Returns error on invalid magic or unsupported version, preventing decryption of corrupted or non-encrypted data.</p>'
    },
    DCHUNK: {
      title: 'Read Ciphertext Chunk',
      badge: 'process', badgeText: 'chunk read',
      body: '<p><code>io.ReadFull(src, buf[:NonceSize+chunkSize+TagSize])</code> reads one ciphertext chunk. Maximum chunk wire size: <code>12 + chunkSize + 16</code> bytes.</p><p>The last chunk may be shorter (<code>io.ErrUnexpectedEOF</code>), but must contain at least <code>NonceSize + TagSize = 28</code> bytes to be valid.</p>'
    },
    VERIFY: {
      title: 'Verify Nonce Matches Index',
      badge: 'filter', badgeText: 'integrity check',
      body: '<p>Byte-by-byte comparison of the embedded nonce against <code>chunkNonce(baseNonce, chunkIdx)</code>. Detects chunk reordering, insertion, or truncation attacks.</p><p>Returns <code>"nonce mismatch at chunk N"</code> on failure. This check is <b>in addition to</b> the GCM authentication tag verification, providing defense in depth.</p>'
    },
    OPEN: {
      title: 'AES-256-GCM Open Chunk',
      badge: 'process', badgeText: 'decryption',
      body: '<p><code>gcm.Open(nil, nonce, ciphertext, nil)</code> decrypts and verifies the authentication tag in a single operation. Returns error if the tag is invalid (tampered data).</p><p>Produces plaintext bytes buffered in <code>decryptReader.buf</code>, drained across subsequent <code>Read()</code> calls for streaming output without full-object buffering.</p>'
    },
    DMORE: {
      title: 'More Chunks?',
      badge: 'decision', badgeText: 'loop control',
      body: '<p>Continues reading ciphertext chunks until <code>io.EOF</code> or <code>io.ErrUnexpectedEOF</code> sets <code>srcDone = true</code>. The decrypted plaintext streams out to the HTTP response writer.</p><p>Chunk index increments after each successful decryption, ensuring nonce derivation stays synchronized with the encryption side.</p>'
    },
    PTOUT: {
      title: 'Plaintext Stream',
      badge: 'success', badgeText: 'decrypt output',
      body: '<p>The <code>decryptReader</code> implements <code>io.Reader</code>, producing plaintext bytes that stream directly to the HTTP response. No full-object buffering required.</p><p>The ETag reported to the client is the one stored on the row at write time, so nothing is recomputed during reads.</p>'
    },
    RANGE: {
      title: 'Range Request (start-end)',
      badge: 'entry', badgeText: 'range entry',
      body: '<p>HTTP Range request (<code>Range: bytes=X-Y</code>) for an encrypted object. The <code>DecryptRange()</code> method handles translating plaintext offsets to ciphertext offsets.</p><p>The base nonce is retrieved from the DB via <code>UnpackKeyData()</code> (stored as <code>baseNonce || wrappedDEK</code>), avoiding a round trip to fetch the ciphertext header.</p>'
    },
    XLATE: {
      title: 'CiphertextRange Translation',
      badge: 'process', badgeText: 'range math',
      body: '<p><code>CiphertextRange(start, end, chunkSize)</code> computes:</p><p><b>startChunk</b> = <code>start / chunkSize</code><br><b>endChunk</b> = <code>end / chunkSize</code><br><b>ctStart</b> = <code>HeaderSize + startChunk * (chunkSize + ChunkOverhead)</code><br><b>ctEnd</b> = <code>HeaderSize + (endChunk+1) * (chunkSize + ChunkOverhead) - 1</code></p><p>Returns <code>RangeResult</code> with <code>BackendRange</code> (e.g., <code>"bytes=32-65595"</code>), <code>SliceStart</code> (offset within first chunk), and <code>SliceLen</code> (plaintext bytes to emit).</p>'
    },
    FETCH: {
      title: 'Fetch Ciphertext Chunks Only',
      badge: 'storage', badgeText: 'partial fetch',
      body: '<p>Backend <code>GetObject</code> with the translated <code>Range: bytes=ctStart-ctEnd</code> header. Only the ciphertext chunks covering the requested plaintext range are fetched &mdash; no header, no unnecessary chunks.</p><p>This is the key efficiency gain: a 1KB range from a 1GB encrypted object fetches at most 2 chunks instead of the entire ciphertext: 128 KiB plus 56 bytes of overhead at the default 64 KiB chunk size, or 2 MiB plus 56 bytes at the 1 MiB maximum.</p>'
    },
    RUNWRAP: {
      title: 'UnwrapDEK via KeyProvider',
      badge: 'storage', badgeText: 'key unwrapping',
      body: '<p>Same <code>provider.UnwrapDEK(ctx, wrappedDEK, keyID)</code> as full decryption. The wrapped DEK and keyID come from the DB object record, not from the ciphertext stream.</p><p>The base nonce was also stored in the DB via <code>PackKeyData</code>, so the header does not need to be fetched from the backend for range requests.</p>'
    },
    RDEC: {
      title: 'Decrypt Relevant Chunks',
      badge: 'process', badgeText: 'chunk decryption',
      body: '<p><code>newDecryptReader(body, dek, baseNonce, chunkSize, startChunk)</code> creates a decrypt reader starting at the correct chunk index for proper nonce derivation.</p><p>Decrypts only the fetched chunks (typically 1-2 for small ranges). Each chunk is verified via GCM tag and nonce match before producing plaintext.</p>'
    },
    SLICE: {
      title: 'Skip + Limit to Byte Range',
      badge: 'filter', badgeText: 'byte slicing',
      body: '<p>Two-step byte-level slicing from <code>DecryptRange()</code>:</p><p>1. <code>io.CopyN(io.Discard, dr, rng.SliceStart)</code> &mdash; skip bytes within the first chunk before the requested offset<br>2. <code>io.LimitReader(dr, rng.SliceLen)</code> &mdash; cap output to exactly the requested byte count</p><p>Result: the caller receives exactly <code>end - start + 1</code> plaintext bytes matching the original Range request semantics.</p>'
    },
    ROUT: {
      title: 'Range Plaintext Bytes',
      badge: 'success', badgeText: 'range output',
      body: '<p>Returns the requested plaintext byte range as an <code>io.Reader</code> along with <code>SliceLen</code> (the content length). Streams directly to the HTTP 206 Partial Content response.</p><p>The <code>Content-Range</code> header uses the original plaintext offsets, transparent to the client &mdash; encryption is entirely server-side.</p>'
    }
  }
};
</script>

## Legend

| Node | Meaning |
|------|---------|
| <span class="dg-swatch dg-entry"></span> | Entry point |
| <span class="dg-swatch dg-filter"></span> | Integrity / filtering |
| <span class="dg-swatch dg-decision"></span> | Decision / branch |
| <span class="dg-swatch dg-process"></span> | Processing step |
| <span class="dg-swatch dg-storage"></span> | Storage / KMS / DB |
| <span class="dg-swatch dg-success"></span> | Output |

