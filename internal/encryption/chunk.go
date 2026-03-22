// -------------------------------------------------------------------------------
// Chunked AES-256-GCM Streaming Encryption
//
// Author: Alex Freidah
//
// Streaming io.Reader wrappers that encrypt and decrypt data in fixed-size
// chunks using AES-256-GCM. Each chunk has an independent nonce derived from a
// base nonce XORed with the chunk index, allowing random access decryption for
// range requests without processing the entire stream.
//
// Wire format:
//   [header 32 bytes][chunk-0][chunk-1]...[chunk-N]
//
// Header: magic "SENC" (4B), version 0x01 (1B), chunk_size big-endian (4B),
// reserved zeros (23B).
//
// Each chunk: nonce (12B) + ciphertext (up to chunk_size bytes) + tag (16B).
// -------------------------------------------------------------------------------

package encryption

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
)

const (
	// HeaderSize is the fixed size of the encryption header.
	HeaderSize = 32

	// NonceSize is the AES-GCM nonce length.
	NonceSize = 12

	// TagSize is the AES-GCM authentication tag length.
	TagSize = 16

	// ChunkOverhead is the per-chunk overhead: nonce + tag.
	ChunkOverhead = NonceSize + TagSize
)

var headerMagic = [4]byte{'S', 'E', 'N', 'C'}

// -------------------------------------------------------------------------
// ENCRYPT READER
// -------------------------------------------------------------------------

// encryptReader wraps an io.Reader and produces chunked AES-256-GCM
// ciphertext. The header is emitted first, followed by encrypted chunks.
type encryptReader struct {
	src       io.Reader
	gcm       cipher.AEAD
	baseNonce []byte
	chunkSize int
	chunkIdx  uint64
	buf       []byte // buffered output waiting to be read
	header    []byte // header bytes not yet consumed
	srcDone   bool
	plainBuf  []byte // reused per Read()
	nonceBuf  []byte // reused per chunk nonce derivation
}

// newEncryptReader creates a streaming encryption reader. The dek must be a
// 256-bit AES key. Plaintext is read from src in chunkSize-byte blocks and
// encrypted independently.
func newEncryptReader(src io.Reader, dek []byte, chunkSize int) (*encryptReader, error) {
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, fmt.Errorf("aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}

	baseNonce := make([]byte, NonceSize)
	if _, err := rand.Read(baseNonce); err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}

	// Build header
	hdr := make([]byte, HeaderSize)
	copy(hdr[0:4], headerMagic[:])
	hdr[4] = 0x01 // version
	binary.BigEndian.PutUint32(hdr[5:9], uint32(chunkSize))
	copy(hdr[9:21], baseNonce)
	// bytes 21-31 reserved (zeros)

	return &encryptReader{
		src:       src,
		gcm:       gcm,
		baseNonce: baseNonce,
		chunkSize: chunkSize,
		header:    hdr,
		plainBuf:  make([]byte, chunkSize),
		nonceBuf:  make([]byte, NonceSize),
	}, nil
}

// Read implements io.Reader. Emits the header followed by encrypted chunks.
func (r *encryptReader) Read(p []byte) (int, error) {
	// Drain header first
	if len(r.header) > 0 {
		n := copy(p, r.header)
		r.header = r.header[n:]
		return n, nil
	}

	// Drain buffered ciphertext
	if len(r.buf) > 0 {
		n := copy(p, r.buf)
		r.buf = r.buf[n:]
		return n, nil
	}

	if r.srcDone {
		return 0, io.EOF
	}

	// Read one plaintext chunk into the reusable buffer
	n, err := io.ReadFull(r.src, r.plainBuf)
	if n == 0 && err != nil {
		r.srcDone = true
		return 0, io.EOF
	}
	plain := r.plainBuf[:n]

	if err == io.ErrUnexpectedEOF || err == io.EOF {
		r.srcDone = true
	} else if err != nil {
		return 0, fmt.Errorf("read plaintext: %w", err)
	}

	// Derive per-chunk nonce into the reusable buffer
	deriveNonce(r.nonceBuf, r.baseNonce, r.chunkIdx)
	r.chunkIdx++

	ct := r.gcm.Seal(nil, r.nonceBuf, plain, nil)
	r.buf = make([]byte, 0, NonceSize+len(ct))
	r.buf = append(r.buf, r.nonceBuf...)
	r.buf = append(r.buf, ct...)

	copied := copy(p, r.buf)
	r.buf = r.buf[copied:]
	return copied, nil
}

// -------------------------------------------------------------------------
// DECRYPT READER
// -------------------------------------------------------------------------

// decryptReader wraps an io.Reader of chunked ciphertext and produces
// plaintext. The header must already be consumed; this reader expects raw
// chunks starting at chunk index startChunk.
type decryptReader struct {
	src       io.Reader
	gcm       cipher.AEAD
	baseNonce []byte
	chunkSize int
	chunkIdx  uint64
	buf       []byte // buffered plaintext
	srcDone   bool
	chunkBuf  []byte // reused per Read()
	nonceBuf  []byte // reused per chunk nonce verification
}

// newDecryptReader creates a streaming decryption reader. The baseNonce is
// extracted from the header. Reads ciphertext chunks from src starting at
// the given chunk index.
func newDecryptReader(src io.Reader, dek []byte, baseNonce []byte, chunkSize int, startChunk uint64) (*decryptReader, error) {
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, fmt.Errorf("aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}

	return &decryptReader{
		src:       src,
		gcm:       gcm,
		baseNonce: baseNonce,
		chunkSize: chunkSize,
		chunkIdx:  startChunk,
		chunkBuf:  make([]byte, NonceSize+chunkSize+TagSize),
		nonceBuf:  make([]byte, NonceSize),
	}, nil
}

// Read implements io.Reader. Decrypts one chunk at a time and returns
// plaintext bytes.
func (r *decryptReader) Read(p []byte) (int, error) {
	// Drain buffered plaintext
	if len(r.buf) > 0 {
		n := copy(p, r.buf)
		r.buf = r.buf[n:]
		return n, nil
	}

	if r.srcDone {
		return 0, io.EOF
	}

	// Read one ciphertext chunk into the reusable buffer
	n, err := io.ReadFull(r.src, r.chunkBuf)
	if n == 0 && err != nil {
		r.srcDone = true
		return 0, io.EOF
	}
	chunk := r.chunkBuf[:n]

	if err == io.ErrUnexpectedEOF || err == io.EOF {
		r.srcDone = true
	} else if err != nil {
		return 0, fmt.Errorf("read ciphertext: %w", err)
	}

	if len(chunk) < NonceSize+TagSize {
		return 0, fmt.Errorf("chunk too short: %d bytes", len(chunk))
	}

	// Verify nonce matches expected chunk index
	nonce := chunk[:NonceSize]
	deriveNonce(r.nonceBuf, r.baseNonce, r.chunkIdx)
	if !bytes.Equal(nonce, r.nonceBuf) {
		return 0, fmt.Errorf("nonce mismatch at chunk %d", r.chunkIdx)
	}
	r.chunkIdx++

	// Decrypt
	plain, err := r.gcm.Open(nil, nonce, chunk[NonceSize:], nil)
	if err != nil {
		return 0, fmt.Errorf("decrypt chunk %d: %w", r.chunkIdx-1, err)
	}

	r.buf = plain
	copied := copy(p, r.buf)
	r.buf = r.buf[copied:]
	return copied, nil
}

// -------------------------------------------------------------------------
// NONCE DERIVATION
// -------------------------------------------------------------------------
//
// SAFETY INVARIANT: AES-GCM requires that the same (key, nonce) pair is
// never used twice. This derivation is safe because:
//
//  1. Each object gets a fresh random DEK (Encryptor.Encrypt generates a
//     new 32-byte key per call — see encryption.go:93).
//  2. Each encrypt call generates a fresh random base nonce (see
//     newEncryptReader — chunk.go:77-78).
//  3. Within a single object, chunk indices are sequential (0, 1, 2, ...),
//     so XOR with the index produces unique nonces per chunk.
//
// Even if the same plaintext is uploaded twice, it gets a different DEK
// and different base nonce. Nonce reuse can only occur if a future code
// change reuses a DEK across objects or re-encrypts with the same DEK
// after a partial failure. The current code never does this — PutObject
// re-encrypts with a fresh DEK on each retry attempt.
//
// If the DEK-per-object invariant is ever relaxed (e.g., for performance),
// this derivation must be replaced with random per-chunk nonces or a
// NIST-compliant counter mode (AES-ECB of the chunk index).

// chunkNonce derives a per-chunk nonce by XORing the chunk index into the
// last 8 bytes of the base nonce. Each chunk gets a unique nonce without
// requiring additional random bytes.
func chunkNonce(base []byte, idx uint64) []byte {
	nonce := make([]byte, NonceSize)
	deriveNonce(nonce, base, idx)
	return nonce
}

// deriveNonce writes a per-chunk nonce into dst by copying the base nonce
// and XORing the chunk index into the last 8 bytes. dst must be at least
// NonceSize bytes. Used by the streaming readers to avoid per-chunk allocation.
func deriveNonce(dst, base []byte, idx uint64) {
	copy(dst, base)
	var idxBytes [8]byte
	binary.BigEndian.PutUint64(idxBytes[:], idx)
	for i := range 8 {
		dst[NonceSize-8+i] ^= idxBytes[i]
	}
}

// -------------------------------------------------------------------------
// HEADER PARSING
// -------------------------------------------------------------------------

// ParseHeader reads and validates the 32-byte encryption header from r.
// Returns the chunk size and base nonce encoded in the header.
func ParseHeader(r io.Reader) (chunkSize int, baseNonce []byte, err error) {
	hdr := make([]byte, HeaderSize)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return 0, nil, fmt.Errorf("read header: %w", err)
	}

	if hdr[0] != headerMagic[0] || hdr[1] != headerMagic[1] ||
		hdr[2] != headerMagic[2] || hdr[3] != headerMagic[3] {
		return 0, nil, fmt.Errorf("invalid encryption header magic")
	}

	if hdr[4] != 0x01 {
		return 0, nil, fmt.Errorf("unsupported encryption version: %d", hdr[4])
	}

	cs := int(binary.BigEndian.Uint32(hdr[5:9]))
	nonce := make([]byte, NonceSize)
	copy(nonce, hdr[9:21])

	return cs, nonce, nil
}
