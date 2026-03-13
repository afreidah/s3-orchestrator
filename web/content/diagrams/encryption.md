---
title: "Encryption Flow"
linkTitle: "Encryption Flow"
weight: 5
---

Envelope encryption and decryption paths for S3 objects using chunked AES-256-GCM with pluggable key providers. **Hover over any component** in the diagram for implementation details.

---

### How it works

When encryption is enabled, every object stored through the S3 Orchestrator is encrypted before it leaves the server. The system uses **envelope encryption** — a two-layer key scheme where each object gets its own throwaway key, and that key is itself encrypted by a master key. Encryption is optional — see the [configuration reference](../../docs/user-guide/) for how to enable it.

#### Key concepts

- **DEK (Data Encryption Key)** — A random 32-byte (256-bit) key generated fresh for every object. This is the key that actually encrypts the data. No two objects share a DEK, so compromising one object's key reveals nothing about any other object.

- **Master key** — A long-lived key managed by a key provider (Vault Transit, a config value, or a key file). The master key never touches the data directly — it only encrypts and decrypts DEKs. This means the master key can be rotated without re-encrypting every object.

- **Nonce** — A 12-byte random value that ensures the same plaintext encrypted with the same key produces different ciphertext each time. The system generates one **base nonce** per object, then mathematically derives a unique nonce for each chunk by XORing the base nonce with the chunk's index number.

- **AES-256-GCM** — The encryption algorithm. GCM mode provides both confidentiality (data is unreadable) and authenticity (any tampering is detected). Each chunk produces a 16-byte authentication tag that acts as a tamper seal.

#### Encrypting an object (write path)

1. **Generate a DEK**: 32 random bytes from the OS cryptographic random source.
2. **Wrap the DEK**: The master key encrypts the DEK via the configured key provider (e.g., Vault Transit API call). This produces a **wrapped DEK** — an opaque blob that can only be unwrapped by the same master key.
3. **Generate a base nonce**: 12 random bytes.
4. **Write the header**: A 32-byte header is prepended to the ciphertext stream — `"SENC"` magic bytes, format version, chunk size, and the base nonce.
5. **Encrypt chunk by chunk** (default 1 MB per chunk):
   - Read up to 1 MB of plaintext.
   - Derive this chunk's nonce: take the base nonce and XOR the chunk index into its last 8 bytes.
   - Encrypt the chunk with AES-256-GCM using the DEK and the derived nonce. This produces ciphertext + a 16-byte auth tag.
   - Write to the output: `nonce (12 bytes) | ciphertext | auth tag (16 bytes)`.
   - Repeat until all plaintext is consumed.
6. **Upload the ciphertext stream** (header + chunks) to the S3 backend.
7. **Store metadata in PostgreSQL**: the wrapped DEK, the master key ID, the base nonce (packed together as `baseNonce || wrappedDEK` in the `encryption_key` column), and the original plaintext size.

The plaintext DEK is never stored anywhere — it exists only in memory during the encryption operation.

#### Decrypting an object (read path)

1. **Retrieve metadata from PostgreSQL**: the wrapped DEK, key ID, and base nonce.
2. **Unwrap the DEK**: The key provider decrypts the wrapped DEK using the master key identified by the stored key ID, recovering the original 32-byte DEK.
3. **Parse the header**: Read the 32-byte header from the ciphertext stream to get the chunk size and base nonce.
4. **Decrypt chunk by chunk**:
   - Read one chunk: `nonce (12B) | ciphertext | auth tag (16B)`.
   - Verify the nonce matches the expected value (base nonce XOR chunk index) — this detects reordering or insertion attacks.
   - Decrypt with AES-256-GCM, which also verifies the auth tag — if the data was tampered with, decryption fails.
   - Stream the plaintext to the client.

#### Range requests (partial reads)

When a client requests a byte range (e.g., `Range: bytes=1000-2000`), the system doesn't need to download and decrypt the entire object:

1. **Translate the plaintext byte range to ciphertext byte range**: figure out which chunks contain the requested bytes, accounting for the 32-byte header and 28-byte overhead per chunk.
2. **Fetch only those chunks** from the S3 backend using a range request.
3. **Decrypt just the fetched chunks** (typically 1–2 chunks for a small range).
4. **Slice the plaintext** to the exact requested bytes and stream them to the client.

The base nonce is stored in the database specifically so range decryption can derive per-chunk nonces without fetching the ciphertext header from the backend.

---

<style>
  #ac-diagram { margin: 1rem 0; }
  #ac-tooltip {
    position: fixed; z-index: 9999; pointer-events: none;
    max-width: 380px; padding: 0.7rem 0.85rem;
    background: #161b22; border: 1px solid #30363d; border-radius: 6px;
    box-shadow: 0 4px 16px rgba(0,0,0,0.4); display: none;
  }
  #ac-tooltip h3 { color: #58a6ff; font-size: 0.85rem; margin: 0 0 0.25rem 0; }
  #ac-tooltip .ac-badge {
    display: inline-block; padding: 1px 7px; border-radius: 4px;
    font-size: 0.6rem; font-weight: 600; margin-bottom: 0.4rem; text-transform: uppercase;
  }
  .ac-badge-entry { background: #1f6feb22; color: #58a6ff; border: 1px solid #58a6ff55; }
  .ac-badge-filter { background: #9e6a0322; color: #d29922; border: 1px solid #d2992255; }
  .ac-badge-decision { background: #58a6ff22; color: #58a6ff; border: 1px solid #58a6ff55; }
  .ac-badge-process { background: #8957e522; color: #bc8cff; border: 1px solid #bc8cff55; }
  .ac-badge-storage { background: #0d419d22; color: #79c0ff; border: 1px solid #79c0ff55; }
  .ac-badge-success { background: #23863622; color: #3fb950; border: 1px solid #3fb95055; }
  .ac-badge-reject { background: #da363322; color: #f85149; border: 1px solid #f8514955; }
  .ac-badge-cleanup { background: #8b949e22; color: #8b949e; border: 1px solid #8b949e55; }
  #ac-tooltip p { font-size: 0.75rem; line-height: 1.4; color: #c9d1d9; margin-bottom: 0.35rem; }
  #ac-tooltip code { background: #21262d; padding: 1px 4px; border-radius: 3px; font-size: 0.7rem; color: #79c0ff; }
  #ac-tooltip .ac-metric { color: #d2a8ff; font-style: italic; font-size: 0.7rem; }
  #ac-diagram .node, #ac-diagram .edgePath, #ac-diagram .edgeLabel { transition: opacity 0.15s, filter 0.15s; }
  #ac-diagram svg.highlighting .node, #ac-diagram svg.highlighting .edgePath, #ac-diagram svg.highlighting .edgeLabel { opacity: 0.12; }
  #ac-diagram svg.highlighting .node.highlight, #ac-diagram svg.highlighting .edgePath.highlight, #ac-diagram svg.highlighting .edgeLabel.highlight { opacity: 1; filter: drop-shadow(0 0 6px rgba(88,166,255,0.5)); }
  #ac-diagram .node { cursor: pointer; }
</style>

<div id="ac-diagram"></div>
<div id="ac-tooltip"></div>

<script src="https://cdn.jsdelivr.net/npm/mermaid@11/dist/mermaid.min.js"></script>
<script>
(function() {
  var diagramSrc = [
    'flowchart TD',
    '    PLAIN([Plaintext\\nStream]):::entry --> DEK[Generate Random\\n256-bit DEK]:::process',
    '    DEK --> WRAP[WrapDEK via\\nKeyProvider]:::storage',
    '    WRAP --> PROVIDER{Key Provider\\nType}:::decision',
    '    PROVIDER -->|Vault Transit| VAULT[Vault Transit\\nEncrypt API]:::storage',
    '    PROVIDER -->|Config / File| LOCAL[AES-GCM Local\\nKey Wrap]:::process',
    '    VAULT --> HDR',
    '    LOCAL --> HDR',
    '',
    '    HDR[Write 32B Header\\nSENC + Nonce]:::process --> CHUNK[Read Plaintext\\nChunk]:::process',
    '    CHUNK --> NONCE[Derive Chunk Nonce\\nBase XOR Index]:::process',
    '    NONCE --> SEAL[AES-256-GCM\\nSeal Chunk]:::process',
    '    SEAL --> MORE{More\\nChunks?}:::decision',
    '    MORE -->|yes| CHUNK',
    '    MORE -->|no| CTOUT([Ciphertext +\\nMetadata]):::success',
    '',
    '    CTIN([Ciphertext\\nFull Object]):::entry --> UNWRAP[UnwrapDEK via\\nKeyProvider]:::storage',
    '    UNWRAP --> PARSE[Parse 32B Header\\nExtract Nonce]:::process',
    '    PARSE --> DCHUNK[Read Ciphertext\\nChunk]:::process',
    '    DCHUNK --> VERIFY[Verify Nonce\\nMatch Index]:::filter',
    '    VERIFY --> OPEN[AES-256-GCM\\nOpen Chunk]:::process',
    '    OPEN --> DMORE{More\\nChunks?}:::decision',
    '    DMORE -->|yes| DCHUNK',
    '    DMORE -->|no| PTOUT([Plaintext\\nStream]):::success',
    '',
    '    RANGE([Range Request\\nstart-end]):::entry --> XLATE[CiphertextRange\\nTranslation]:::process',
    '    XLATE --> FETCH[Fetch Ciphertext\\nChunks Only]:::storage',
    '    FETCH --> RUNWRAP[UnwrapDEK via\\nKeyProvider]:::storage',
    '    RUNWRAP --> RDEC[Decrypt Relevant\\nChunks]:::process',
    '    RDEC --> SLICE[Skip + Limit\\nto Byte Range]:::filter',
    '    SLICE --> ROUT([Range Plaintext\\nBytes]):::success',
    '',
    '    classDef entry fill:#1f6feb,stroke:#1f6feb,color:#fff,font-weight:bold',
    '    classDef filter fill:#9e6a03,stroke:#d29922,color:#fff',
    '    classDef decision fill:#21262d,stroke:#58a6ff,color:#e6edf3,font-size:11px',
    '    classDef process fill:#8957e5,stroke:#bc8cff,color:#fff',
    '    classDef storage fill:#0d419d,stroke:#58a6ff,color:#c9d1d9',
    '    classDef success fill:#238636,stroke:#3fb950,color:#fff,font-weight:bold',
    '    classDef reject fill:#da3633,stroke:#f85149,color:#fff,font-weight:bold',
    '    classDef cleanup fill:#21262d,stroke:#8b949e,color:#e6edf3'
  ].join('\n');

  mermaid.initialize({
    startOnLoad: false, theme: 'dark',
    flowchart: { nodeSpacing: 14, rankSpacing: 22, curve: 'basis', padding: 5, diagramPadding: 8, useMaxWidth: true }
  });

  mermaid.render('enc-mermaid-svg', diagramSrc).then(function(result) {
    document.getElementById('ac-diagram').innerHTML = result.svg;
    wireUpInteractivity();
  });

  var nodeInfo = {
    PLAIN: {
      title: 'Plaintext Stream',
      badge: 'entry', badgeText: 'encrypt entry',
      body: '<p>Incoming plaintext body from <code>PutObject</code>. The body is passed through <code>io.TeeReader</code> to simultaneously compute an MD5 digest for the client-facing ETag.</p><p>The <code>Encryptor.Encrypt(ctx, body, plaintextSize)</code> method orchestrates the full envelope encryption pipeline.</p>'
    },
    DEK: {
      title: 'Generate Random 256-bit DEK',
      badge: 'process', badgeText: 'key generation',
      body: '<p>Generates a 32-byte (256-bit) Data Encryption Key using <code>crypto/rand.Read(dek)</code>. Each object gets a unique DEK &mdash; no key reuse across objects.</p><p>On write retries (failover to another backend), a <b>fresh DEK and nonce</b> are generated, producing different ciphertext for each attempt.</p>'
    },
    WRAP: {
      title: 'WrapDEK via KeyProvider',
      badge: 'storage', badgeText: 'key wrapping',
      body: '<p><code>provider.WrapDEK(ctx, dek)</code> encrypts the plaintext DEK with the master key. Returns <code>(wrappedDEK, keyID, error)</code>.</p><p>The <code>keyID</code> identifies which master key was used, enabling key rotation via <code>MultiKeyProvider</code>. The wrapped DEK and keyID are stored in PostgreSQL alongside the object record.</p><p class="ac-metric">Metric: s3proxy_encryption_ops_total{operation="encrypt"}</p>'
    },
    PROVIDER: {
      title: 'Key Provider Type',
      badge: 'decision', badgeText: 'provider selection',
      body: '<p>Resolved at startup by <code>NewKeyProviderFromConfig()</code>. Three provider types:</p><p><b>Vault Transit</b>: master key never leaves Vault; DEK wrapped via HTTP API<br><b>ConfigKeyProvider</b>: base64-encoded 256-bit key in config file<br><b>FileKeyProvider</b>: raw 32-byte key read from disk</p><p><code>MultiKeyProvider</code> wraps any primary with fallback keys for rotation: <code>WrapDEK</code> always uses primary; <code>UnwrapDEK</code> resolves by <code>keyID</code>.</p>'
    },
    VAULT: {
      title: 'Vault Transit Encrypt API',
      badge: 'storage', badgeText: 'external KMS',
      body: '<p>POST to <code>{vault}/v1/{mount}/encrypt/{keyName}</code> with base64-encoded DEK as plaintext payload. <code>X-Vault-Token</code> header for auth.</p><p>Returns a Vault ciphertext blob (e.g., <code>vault:v1:...</code>) stored as the wrapped DEK. The master key <b>never leaves Vault</b>.</p><p>KeyID format: <code>vault:{mountPath}/{keyName}</code>. HTTP client has 10s timeout and optional custom CA cert for TLS.</p>'
    },
    LOCAL: {
      title: 'AES-GCM Local Key Wrap',
      badge: 'process', badgeText: 'local wrapping',
      body: '<p><code>aesGCMWrap(masterKey, dek)</code>: creates AES-256-GCM cipher from the 32-byte master key, generates a random 12-byte nonce, and seals the DEK.</p><p>Output format: <code>nonce (12B) || ciphertext+tag</code>. The <code>aesGCMUnwrap</code> reversal splits on the nonce boundary.</p><p>Used by both <code>ConfigKeyProvider</code> (key from config/env) and <code>FileKeyProvider</code> (raw key from disk file).</p>'
    },
    HDR: {
      title: 'Write 32-Byte Header',
      badge: 'process', badgeText: 'wire format',
      body: '<p>The <code>encryptReader</code> emits a 32-byte header before any ciphertext chunks:</p><p><code>SENC</code> magic (4B) + version <code>0x01</code> (1B) + chunk_size big-endian (4B) + baseNonce (12B) + reserved zeros (11B)</p><p>The base nonce is also stored in the DB via <code>PackKeyData(baseNonce || wrappedDEK)</code> for range decryption without re-fetching the header from the backend.</p>'
    },
    CHUNK: {
      title: 'Read Plaintext Chunk',
      badge: 'process', badgeText: 'chunking',
      body: '<p><code>io.ReadFull(src, plain[:chunkSize])</code> reads up to <code>chunkSize</code> bytes (default 1MB = 1048576 bytes). The last chunk may be shorter.</p><p>Streaming design: <code>encryptReader</code> implements <code>io.Reader</code>, encrypting one chunk per <code>Read()</code> call. No need to buffer the entire object in memory.</p>'
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
      body: '<p>The <code>encryptReader.Read()</code> loop continues until <code>io.ReadFull</code> returns <code>io.EOF</code> or <code>io.ErrUnexpectedEOF</code> (last partial chunk), at which point <code>srcDone = true</code>.</p><p>The <code>md5FinalizingReader</code> wrapper captures the plaintext MD5 hex digest on EOF and stores it in <code>EncryptResult.PlaintextMD5</code> for the client-facing ETag.</p>'
    },
    CTOUT: {
      title: 'Ciphertext + Metadata',
      badge: 'success', badgeText: 'encrypt output',
      body: '<p><code>EncryptResult</code> contains everything needed for storage:</p><p><b>Body</b>: streaming ciphertext reader (header + chunks)<br><b>CiphertextSize</b>: pre-computed via <code>ciphertextSizeExact()</code><br><b>WrappedDEK</b>: encrypted DEK bytes<br><b>KeyID</b>: master key identifier<br><b>BaseNonce</b>: 12-byte nonce for range decryption<br><b>PlaintextMD5</b>: hex MD5 (finalized on EOF)</p><p>DB stores <code>PackKeyData(baseNonce || wrappedDEK)</code> and <code>keyID</code> in the object record.</p>'
    },
    CTIN: {
      title: 'Ciphertext (Full Object)',
      badge: 'entry', badgeText: 'decrypt entry',
      body: '<p>Full ciphertext stream fetched from the S3 backend via <code>GetObject</code>. The <code>Encryptor.Decrypt(ctx, body, wrappedDEK, keyID)</code> method handles the complete decryption flow.</p><p>The wrapped DEK and keyID are retrieved from PostgreSQL along with the object location metadata.</p>'
    },
    UNWRAP: {
      title: 'UnwrapDEK via KeyProvider',
      badge: 'storage', badgeText: 'key unwrapping',
      body: '<p><code>provider.UnwrapDEK(ctx, wrappedDEK, keyID)</code> recovers the plaintext 32-byte DEK.</p><p>For <code>MultiKeyProvider</code>: if <code>keyID</code> matches the primary key, uses primary; otherwise looks up in the <code>previous</code> map by keyID. Falls back to primary as best-effort for unknown keyIDs.</p><p>Vault Transit: POST to <code>{vault}/v1/{mount}/decrypt/{keyName}</code>, returns base64-decoded plaintext DEK.</p><p class="ac-metric">Metric: s3proxy_encryption_ops_total{operation="decrypt"}</p>'
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
      body: '<p>The <code>decryptReader</code> implements <code>io.Reader</code>, producing plaintext bytes that stream directly to the HTTP response. No full-object buffering required.</p><p>The client-facing ETag was pre-computed from the plaintext MD5 during encryption and stored in the DB, so no recomputation is needed during reads.</p>'
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
      body: '<p>Backend <code>GetObject</code> with the translated <code>Range: bytes=ctStart-ctEnd</code> header. Only the ciphertext chunks covering the requested plaintext range are fetched &mdash; no header, no unnecessary chunks.</p><p>This is the key efficiency gain: a 1KB range from a 1GB encrypted object fetches at most 2 chunks (2MB + 56B overhead) instead of the entire ciphertext.</p>'
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
  };

  var tooltip = document.getElementById('ac-tooltip');
  var mouseX = 0, mouseY = 0;
  document.addEventListener('mousemove', function(e) {
    mouseX = e.clientX; mouseY = e.clientY;
    if (tooltip.style.display === 'block') positionTooltip();
  });
  function positionTooltip() {
    var pad = 12, x = mouseX + pad, y = mouseY + pad;
    if (x + tooltip.offsetWidth > window.innerWidth - pad) x = mouseX - tooltip.offsetWidth - pad;
    if (y + tooltip.offsetHeight > window.innerHeight - pad) y = mouseY - tooltip.offsetHeight - pad;
    tooltip.style.left = x + 'px'; tooltip.style.top = y + 'px';
  }
  function showInfo(id) {
    var info = nodeInfo[id];
    if (!info) { tooltip.style.display = 'none'; return; }
    tooltip.innerHTML = '<h3>' + info.title + '</h3><span class="ac-badge ac-badge-' + info.badge + '">' + info.badgeText + '</span>' + info.body;
    tooltip.style.display = 'block'; positionTooltip();
  }
  function clearInfo() { tooltip.style.display = 'none'; }

  function wireUpInteractivity() {
    var svg = document.querySelector('#ac-diagram svg');
    if (!svg) return;
    var adj = {}, edgeMap = {};
    svg.querySelectorAll('.edgePath').forEach(function(ep, i) {
      var cls = ep.getAttribute('class') || '';
      var m = cls.match(/LS-(\S+)/), m2 = cls.match(/LE-(\S+)/);
      if (!m || !m2) return;
      edgeMap[i] = { from: m[1], to: m2[1], path: ep, label: svg.querySelectorAll('.edgeLabel')[i] };
      (adj[m[1]] = adj[m[1]] || []).push(i);
    });
    function bfs(startId, adjacency, getNext) {
      var visited = new Set([startId]), edges = new Set(), queue = [startId];
      while (queue.length) { var cur = queue.shift(); (adjacency[cur] || []).forEach(function(ei) {
        edges.add(ei); var next = getNext(edgeMap[ei]);
        if (!visited.has(next)) { visited.add(next); queue.push(next); }
      }); } return { nodes: visited, edges: edges };
    }
    var radj = {};
    Object.keys(edgeMap).forEach(function(i) { var e = edgeMap[i]; (radj[e.to] = radj[e.to] || []).push(Number(i)); });
    svg.querySelectorAll('.node').forEach(function(node) {
      var id = node.id.replace(/^flowchart-/, '').replace(/-\d+$/, '');
      node.addEventListener('mouseenter', function() {
        svg.classList.add('highlighting');
        var fwd = bfs(id, adj, function(e) { return e.to; });
        var bwd = bfs(id, radj, function(e) { return e.from; });
        var allNodes = new Set([...fwd.nodes, ...bwd.nodes]);
        var allEdges = new Set([...fwd.edges, ...bwd.edges]);
        svg.querySelectorAll('.node').forEach(function(n) {
          n.classList.toggle('highlight', allNodes.has(n.id.replace(/^flowchart-/, '').replace(/-\d+$/, '')));
        });
        Object.keys(edgeMap).forEach(function(i) {
          var hl = allEdges.has(Number(i));
          edgeMap[i].path.classList.toggle('highlight', hl);
          if (edgeMap[i].label) edgeMap[i].label.classList.toggle('highlight', hl);
        });
        showInfo(id);
      });
      node.addEventListener('mouseleave', function() {
        svg.classList.remove('highlighting');
        svg.querySelectorAll('.highlight').forEach(function(el) { el.classList.remove('highlight'); });
        clearInfo();
      });
    });
  }
})();
</script>

## Legend

| Color | Meaning |
|-------|---------|
| <span style="color:#1f6feb">**Blue**</span> | Entry point |
| <span style="color:#d29922">**Amber**</span> | Integrity / filtering |
| <span style="color:#58a6ff">**Blue border**</span> | Decision / branch |
| <span style="color:#bc8cff">**Purple**</span> | Processing step |
| <span style="color:#79c0ff">**Dark blue**</span> | Storage / KMS / DB |
| <span style="color:#3fb950">**Green**</span> | Output |

