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

	// Read one plaintext chunk
	plain := make([]byte, r.chunkSize)
	n, err := io.ReadFull(r.src, plain)
	if n == 0 && err != nil {
		r.srcDone = true
		return 0, io.EOF
	}
	plain = plain[:n]

	if err == io.ErrUnexpectedEOF || err == io.EOF {
		r.srcDone = true
	} else if err != nil {
		return 0, fmt.Errorf("read plaintext: %w", err)
	}

	// Derive per-chunk nonce
	nonce := chunkNonce(r.baseNonce, r.chunkIdx)
	r.chunkIdx++

	ct := r.gcm.Seal(nil, nonce, plain, nil)
	r.buf = make([]byte, 0, len(nonce)+len(ct))
	r.buf = append(r.buf, nonce...)
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

	// Read one ciphertext chunk: nonce + (up to chunkSize + tagSize) bytes
	chunkBuf := make([]byte, NonceSize+r.chunkSize+TagSize)
	n, err := io.ReadFull(r.src, chunkBuf)
	if n == 0 && err != nil {
		r.srcDone = true
		return 0, io.EOF
	}
	chunkBuf = chunkBuf[:n]

	if err == io.ErrUnexpectedEOF || err == io.EOF {
		r.srcDone = true
	} else if err != nil {
		return 0, fmt.Errorf("read ciphertext: %w", err)
	}

	if len(chunkBuf) < NonceSize+TagSize {
		return 0, fmt.Errorf("chunk too short: %d bytes", len(chunkBuf))
	}

	// Verify nonce matches expected chunk index
	nonce := chunkBuf[:NonceSize]
	expected := chunkNonce(r.baseNonce, r.chunkIdx)
	for i := range nonce {
		if nonce[i] != expected[i] {
			return 0, fmt.Errorf("nonce mismatch at chunk %d", r.chunkIdx)
		}
	}
	r.chunkIdx++

	// Decrypt
	plain, err := r.gcm.Open(nil, nonce, chunkBuf[NonceSize:], nil)
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

// chunkNonce derives a per-chunk nonce by XORing the chunk index into the
// last 8 bytes of the base nonce. Each chunk gets a unique nonce without
// requiring additional random bytes.
func chunkNonce(base []byte, idx uint64) []byte {
	nonce := make([]byte, NonceSize)
	copy(nonce, base)

	// XOR chunk index into the last 8 bytes
	var idxBytes [8]byte
	binary.BigEndian.PutUint64(idxBytes[:], idx)
	for i := 0; i < 8; i++ {
		nonce[NonceSize-8+i] ^= idxBytes[i]
	}
	return nonce
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
