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
	"errors"
	"fmt"
	"io"
)

// HeaderSize and related constants used by this package.
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

// headerMagic is the four-byte signature ("SENC") prepended to every
// encrypted object's ciphertext. Lets the decrypt path reject non-
// orchestrator data before consuming a wrap call, and lets future
// format upgrades introduce a versioned header without ambiguity.
var headerMagic = [4]byte{'S', 'E', 'N', 'C'}

// -------------------------------------------------------------------------
// CHUNK BUFFERS
// -------------------------------------------------------------------------

// chunkBuffers is the per-stream byte-buffer set shared by encryptReader
// and decryptReader. Pooled on Encryptor.bufPool so the steady-state
// Read path allocates nothing.
type chunkBuffers struct {
	plain  []byte // chunkSize cap
	framed []byte // NonceSize + chunkSize + TagSize cap
	nonce  []byte // NonceSize cap
	header []byte // HeaderSize cap; encrypt path only
}

func newChunkBuffers(chunkSize int) *chunkBuffers {
	return &chunkBuffers{
		plain:  make([]byte, chunkSize),
		framed: make([]byte, 0, NonceSize+chunkSize+TagSize),
		nonce:  make([]byte, NonceSize),
		header: make([]byte, HeaderSize),
	}
}

// -------------------------------------------------------------------------
// ENCRYPT READER
// -------------------------------------------------------------------------

// encryptReader wraps an io.Reader and produces chunked AES-256-GCM
// ciphertext. The header is emitted first, followed by encrypted chunks.
// bufs are borrowed from the caller (typically Encryptor.bufPool); when
// release is non-nil it fires once at EOF to return them to the pool.
type encryptReader struct {
	src       io.Reader
	gcm       cipher.AEAD
	baseNonce []byte
	chunkSize int
	chunkIdx  uint64
	bufs      *chunkBuffers
	release   func()
	buf       []byte // view into bufs.framed showing the unconsumed prefix
	header    []byte // view into bufs.header showing header bytes not yet emitted
	srcDone   bool
}

// newEncryptReader creates a streaming encryption reader. The dek must be a
// 256-bit AES key. Plaintext is read from src in chunkSize-byte blocks and
// encrypted independently. bufs is borrowed for the lifetime of the reader;
// release (if non-nil) fires once at EOF to return them.
func newEncryptReader(src io.Reader, dek []byte, chunkSize int, bufs *chunkBuffers, release func()) (*encryptReader, error) {
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

	// Build header into the reusable header buffer.
	hdr := bufs.header[:HeaderSize]
	copy(hdr[0:4], headerMagic[:])
	hdr[4] = 0x01                                           // version
	binary.BigEndian.PutUint32(hdr[5:9], uint32(chunkSize)) //nolint:gosec // G115: chunkSize validated <= 1MB in config
	copy(hdr[9:21], baseNonce)
	for i := 21; i < HeaderSize; i++ {
		hdr[i] = 0 // reserved
	}

	return &encryptReader{
		src:       src,
		gcm:       gcm,
		baseNonce: baseNonce,
		chunkSize: chunkSize,
		bufs:      bufs,
		release:   release,
		header:    hdr,
	}, nil
}

// returnBufs fires the release callback at most once. Called from the
// Read path the moment EOF is observed so the buffers go back to the
// pool without waiting on a separate Close. Callers that abandon the
// reader before EOF leak the bufs to GC, which the pool tolerates.
func (r *encryptReader) returnBufs() {
	if r.release == nil {
		return
	}
	release := r.release
	r.release = nil
	r.bufs = nil
	r.buf = nil
	r.header = nil
	release()
}

// readChunk fills buf from src and reports which of the three endings
// io.ReadFull distinguishes it hit: a clean end of stream (0, io.EOF), a
// partial final chunk (n>0, io.ErrUnexpectedEOF), or a real error from the
// source. done covers the first two; the error is returned as io.EOF only for
// the first.
//
// The bug class this defends against is squashing the third case to io.EOF,
// which would let a transient backend failure land in storage as a
// truncated-but-valid object. Both directions of the stream read through here
// so neither can drift into doing that.
func readChunk(src io.Reader, buf []byte, what string) (n int, done bool, err error) {
	n, err = io.ReadFull(src, buf)
	switch {
	case n == 0 && errors.Is(err, io.EOF):
		return 0, true, io.EOF
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		return n, true, nil
	case err != nil:
		return 0, false, fmt.Errorf("read %s: %w", what, err)
	}
	return n, false, nil
}

// Read implements io.Reader. Emits the header followed by encrypted chunks.
func (r *encryptReader) Read(p []byte) (int, error) {
	if r.bufs == nil {
		return 0, io.EOF
	}

	// Drain header first.
	if len(r.header) > 0 {
		n := copy(p, r.header)
		r.header = r.header[n:]
		return n, nil
	}

	// Drain buffered ciphertext.
	if len(r.buf) > 0 {
		n := copy(p, r.buf)
		r.buf = r.buf[n:]
		return n, nil
	}

	if r.srcDone {
		r.returnBufs()
		return 0, io.EOF
	}

	n, done, err := readChunk(r.src, r.bufs.plain, "plaintext")
	if done {
		r.srcDone = true
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			r.returnBufs()
		}
		return 0, err
	}
	plain := r.bufs.plain[:n]

	// Derive per-chunk nonce into the reusable buffer.
	deriveNonce(r.bufs.nonce, r.baseNonce, r.chunkIdx)
	r.chunkIdx++

	// Seal into the reusable framed buffer: prepend the nonce, then
	// let gcm.Seal append ciphertext+tag. The buffer's capacity is
	// pre-sized so neither the append nor Seal reallocates. Passing
	// nil dst would force gcm.sliceForAppend to allocate per chunk,
	// which used to dominate this service's allocator profile.
	r.bufs.framed = append(r.bufs.framed[:0], r.bufs.nonce...)
	r.bufs.framed = r.gcm.Seal(r.bufs.framed, r.bufs.nonce, plain, nil)
	r.buf = r.bufs.framed

	copied := copy(p, r.buf)
	r.buf = r.buf[copied:]
	return copied, nil
}

// -------------------------------------------------------------------------
// DECRYPT READER
// -------------------------------------------------------------------------

// decryptReader wraps an io.Reader of chunked ciphertext and produces
// plaintext. The header must already be consumed; this reader expects raw
// chunks starting at chunk index startChunk. bufs are borrowed from the
// caller (typically Encryptor.bufPool); when release is non-nil it fires
// once at EOF to return them to the pool.
type decryptReader struct {
	src       io.Reader
	gcm       cipher.AEAD
	baseNonce []byte
	chunkSize int
	chunkIdx  uint64
	bufs      *chunkBuffers
	release   func()
	buf       []byte // view into bufs.plain showing the unconsumed prefix
	srcDone   bool
}

// newDecryptReader creates a streaming decryption reader. The baseNonce is
// extracted from the header. Reads ciphertext chunks from src starting at
// the given chunk index. bufs is borrowed for the lifetime of the reader;
// release (if non-nil) fires once at EOF to return them.
func newDecryptReader(src io.Reader, dek []byte, baseNonce []byte, chunkSize int, startChunk uint64, bufs *chunkBuffers, release func()) (*decryptReader, error) {
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
		bufs:      bufs,
		release:   release,
	}, nil
}

func (r *decryptReader) returnBufs() {
	if r.release == nil {
		return
	}
	release := r.release
	r.release = nil
	r.bufs = nil
	r.buf = nil
	release()
}

// Read implements io.Reader. Decrypts one chunk at a time and returns
// plaintext bytes.
func (r *decryptReader) Read(p []byte) (int, error) {
	if r.bufs == nil {
		return 0, io.EOF
	}

	// Drain buffered plaintext.
	if len(r.buf) > 0 {
		n := copy(p, r.buf)
		r.buf = r.buf[n:]
		return n, nil
	}

	if r.srcDone {
		r.returnBufs()
		return 0, io.EOF
	}

	chunkBuf := r.bufs.framed[:cap(r.bufs.framed)]
	n, done, err := readChunk(r.src, chunkBuf, "ciphertext")
	if done {
		r.srcDone = true
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			r.returnBufs()
		}
		return 0, err
	}
	chunk := chunkBuf[:n]

	if len(chunk) < NonceSize+TagSize {
		return 0, fmt.Errorf("chunk too short: %d bytes", len(chunk))
	}

	// Verify nonce matches expected chunk index.
	nonce := chunk[:NonceSize]
	deriveNonce(r.bufs.nonce, r.baseNonce, r.chunkIdx)
	if !bytes.Equal(nonce, r.bufs.nonce) {
		return 0, fmt.Errorf("nonce mismatch at chunk %d", r.chunkIdx)
	}
	r.chunkIdx++

	// Open into the reusable plain buffer. bufs.plain has capacity
	// chunkSize so gcm.Open's appended plaintext fits without
	// reallocating; passing nil dst would force a fresh slice
	// allocation per chunk via crypto/.../gcm.sliceForAppend.
	plain, err := r.gcm.Open(r.bufs.plain[:0], nonce, chunk[NonceSize:], nil)
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
//     new 32-byte key per call  -  see encryption.go:93).
//  2. Each encrypt call generates a fresh random base nonce (see
//     newEncryptReader  -  chunk.go:77-78).
//  3. Within a single object, chunk indices are sequential (0, 1, 2, ...),
//     so XOR with the index produces unique nonces per chunk.
//
// Even if the same plaintext is uploaded twice, it gets a different DEK
// and different base nonce. Nonce reuse can only occur if a future code
// change reuses a DEK across objects or re-encrypts with the same DEK
// after a partial failure. The current code never does this  -  PutObject
// re-encrypts with a fresh DEK on each retry attempt.
//
// If the DEK-per-object invariant is ever relaxed (e.g., for performance),
// this derivation must be replaced with random per-chunk nonces or a
// NIST-compliant counter mode (AES-ECB of the chunk index).

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
	return ParseHeaderBytes(hdr)
}

// ParseHeaderBytes validates an already-read 32-byte encryption header and
// returns the chunk size and base nonce encoded in it. Callers that fetched
// the header as part of a larger ranged read use this to avoid a second read.
func ParseHeaderBytes(hdr []byte) (chunkSize int, baseNonce []byte, err error) {
	if len(hdr) < HeaderSize {
		return 0, nil, fmt.Errorf("short encryption header: %d bytes", len(hdr))
	}

	if !HasEnvelopeMagic(hdr) {
		return 0, nil, fmt.Errorf("invalid encryption header magic")
	}

	if hdr[4] != 0x01 {
		return 0, nil, fmt.Errorf("unsupported encryption version: %d", hdr[4])
	}

	cs := int(binary.BigEndian.Uint32(hdr[5:9]))
	if cs <= 0 {
		return 0, nil, fmt.Errorf("invalid chunk size in header: %d", cs)
	}
	nonce := make([]byte, NonceSize)
	copy(nonce, hdr[9:21])

	return cs, nonce, nil
}

// SameEncryptionOperation reports whether an envelope header read off a
// backend was produced by the same encryption operation as the stored key
// blob packed by PackKeyData.
//
// The base nonce is drawn fresh from crypto/rand for every encryption run
// (see newEncryptReader), and copies of an object reproduce its ciphertext
// byte for byte, so a matching nonce means the blob's DEK is the one that
// encrypted these bytes. A separate write of the same key gets a different
// nonce, which is what makes this safe to use for deciding whether a stray
// backend object may adopt a sibling row's key.
//
// This establishes identity, not authenticity: it assumes the backend holds
// what the orchestrator wrote. Bytes that lie about their header still fail
// the AEAD tag on the first real read.
func SameEncryptionOperation(header, packedKey []byte) bool {
	_, headerNonce, err := ParseHeaderBytes(header)
	if err != nil {
		return false
	}
	storedNonce, _, err := UnpackKeyData(packedKey)
	if err != nil {
		return false
	}
	return bytes.Equal(headerNonce, storedNonce)
}

// HasEnvelopeMagic reports whether b begins with the envelope signature.
// b shorter than the signature is never an envelope.
func HasEnvelopeMagic(b []byte) bool {
	if len(b) < len(headerMagic) {
		return false
	}
	return b[0] == headerMagic[0] && b[1] == headerMagic[1] &&
		b[2] == headerMagic[2] && b[3] == headerMagic[3]
}

// PeekEnvelope reports whether r's stream begins with the envelope signature,
// returning a reader that replays the bytes it consumed so the caller can go
// on reading from the start.
//
// This is how a caller checks that a row's encrypted flag agrees with the
// bytes actually stored. The two disagreeing means either ciphertext would be
// served as plaintext or plaintext decrypted as ciphertext, both of which are
// worth failing on rather than guessing.
//
// A short stream is reported as not-an-envelope with the bytes replayed; a
// read error is returned with a reader that still replays whatever arrived.
func PeekEnvelope(r io.Reader) (bool, io.Reader, error) {
	buf := make([]byte, len(headerMagic))
	n, err := io.ReadFull(r, buf)
	replayed := io.MultiReader(bytes.NewReader(buf[:n]), r)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return false, replayed, nil
	}
	if err != nil {
		return false, replayed, fmt.Errorf("peek encryption header: %w", err)
	}
	return HasEnvelopeMagic(buf), replayed, nil
}
