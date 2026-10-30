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
// base nonce (12B), reserved zeros (11B).
//
// Each chunk: nonce (12B) + ciphertext (up to chunk_size bytes) + tag (16B).
// -------------------------------------------------------------------------------

package encryption

import (
	"bytes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// HeaderSize and the AES-GCM sizes the envelope format is built from. Every
// chunk carries its own nonce and tag, so ChunkOverhead is what a chunk costs
// on top of its plaintext.
const (
	HeaderSize    = 32
	NonceSize     = 12
	TagSize       = 16
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
// CHUNK STREAM
// -------------------------------------------------------------------------

// chunkStream is the state the encrypt and decrypt readers share. bufs are
// borrowed from the caller (typically Encryptor.bufPool); when release is
// non-nil it fires once at EOF to return them. A reader abandoned before EOF
// leaks them to GC, which the pool tolerates.
type chunkStream struct {
	src       io.Reader
	gcm       cipher.AEAD
	baseNonce []byte
	chunkSize int
	chunkIdx  uint64
	bufs      *chunkBuffers
	release   func()
	buf       []byte // the current chunk's output not yet returned
	srcDone   bool
}

// returnBufs drops the buffers and fires release at most once, the moment
// EOF is observed, so they go back to the pool without waiting on a Close.
func (s *chunkStream) returnBufs() {
	release := s.release
	s.release, s.bufs, s.buf = nil, nil, nil
	if release != nil {
		release()
	}
}

// drain answers a Read from what is already buffered, or with io.EOF once
// the stream has ended. ok is false when the caller has to produce the next
// chunk.
func (s *chunkStream) drain(p []byte) (n int, ok bool, err error) {
	switch {
	case s.bufs == nil:
		return 0, true, io.EOF
	case len(s.buf) > 0:
		n, err = s.emit(p)
		return n, true, err
	case s.srcDone:
		s.returnBufs()
		return 0, true, io.EOF
	}
	return 0, false, nil
}

// emit copies the buffered output into p.
func (s *chunkStream) emit(p []byte) (int, error) {
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

// fill reads the next chunk into buf. A clean end of stream is io.EOF, and a
// short final chunk marks the source done. A source error must never become
// io.EOF, or a transient backend failure would be stored as a truncated but
// valid object.
func (s *chunkStream) fill(buf []byte, what string) (int, error) {
	n, err := io.ReadFull(s.src, buf)
	switch {
	case n == 0 && errors.Is(err, io.EOF):
		s.returnBufs()
		return 0, io.EOF
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		s.srcDone = true
	case err != nil:
		return 0, fmt.Errorf("read %s: %w", what, err)
	}
	return n, nil
}

// -------------------------------------------------------------------------
// ENCRYPT READER
// -------------------------------------------------------------------------

// encryptReader wraps an io.Reader and produces chunked AES-256-GCM
// ciphertext. The header is emitted first, followed by encrypted chunks.
type encryptReader struct {
	chunkStream
	header []byte // view into bufs.header showing header bytes not yet emitted
}

// newEncryptReader creates a streaming encryption reader. The dek must be a
// 256-bit AES key. Plaintext is read from src in chunkSize-byte blocks and
// encrypted independently. bufs is borrowed for the lifetime of the reader;
// release (if non-nil) fires once at EOF to return them.
func newEncryptReader(src io.Reader, dek []byte, chunkSize int, bufs *chunkBuffers, release func()) (*encryptReader, error) {
	gcm, err := newGCM(dek)
	if err != nil {
		return nil, err
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
		chunkStream: chunkStream{
			src:       src,
			gcm:       gcm,
			baseNonce: baseNonce,
			chunkSize: chunkSize,
			bufs:      bufs,
			release:   release,
		},
		header: hdr,
	}, nil
}

// Read implements io.Reader. Emits the header followed by encrypted chunks.
func (r *encryptReader) Read(p []byte) (int, error) {
	if len(r.header) > 0 {
		n := copy(p, r.header)
		r.header = r.header[n:]
		return n, nil
	}
	if n, ok, err := r.drain(p); ok {
		return n, err
	}

	n, err := r.fill(r.bufs.plain, "plaintext")
	if err != nil {
		return 0, err
	}
	plain := r.bufs.plain[:n]

	// Derive per-chunk nonce into the reusable buffer.
	deriveNonce(r.bufs.nonce, r.baseNonce, r.chunkIdx)
	r.chunkIdx++

	// Seal into the pre-sized framed buffer after the nonce; a nil dst
	// would allocate per chunk.
	r.bufs.framed = append(r.bufs.framed[:0], r.bufs.nonce...)
	r.bufs.framed = r.gcm.Seal(r.bufs.framed, r.bufs.nonce, plain, nil)
	r.buf = r.bufs.framed
	return r.emit(p)
}

// -------------------------------------------------------------------------
// DECRYPT READER
// -------------------------------------------------------------------------

// decryptReader wraps an io.Reader of chunked ciphertext and produces
// plaintext. The header must already be consumed; this reader expects raw
// chunks of chunkSize starting at chunk index startChunk.
type decryptReader struct {
	chunkStream
}

// newDecryptReader creates a streaming decryption reader. The baseNonce is
// extracted from the header. Reads ciphertext chunks from src starting at
// the given chunk index. bufs is borrowed for the lifetime of the reader;
// release (if non-nil) fires once at EOF to return them.
func newDecryptReader(src io.Reader, dek []byte, baseNonce []byte, chunkSize int, startChunk uint64, bufs *chunkBuffers, release func()) (*decryptReader, error) {
	gcm, err := newGCM(dek)
	if err != nil {
		return nil, err
	}

	return &decryptReader{chunkStream{
		src:       src,
		gcm:       gcm,
		baseNonce: baseNonce,
		chunkSize: chunkSize,
		chunkIdx:  startChunk,
		bufs:      bufs,
		release:   release,
	}}, nil
}

// Read implements io.Reader. Decrypts one chunk at a time and returns
// plaintext bytes.
func (r *decryptReader) Read(p []byte) (int, error) {
	if n, ok, err := r.drain(p); ok {
		return n, err
	}

	// Pooled buffers can be larger than this object's chunks.
	chunkBuf := r.bufs.framed[:NonceSize+r.chunkSize+TagSize]
	n, err := r.fill(chunkBuf, "ciphertext")
	if err != nil {
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
	return r.emit(p)
}

// -------------------------------------------------------------------------
// NONCE DERIVATION
// -------------------------------------------------------------------------
//
// SAFETY INVARIANT: AES-GCM requires that a (key, nonce) pair is never used
// twice. This derivation is safe because Encryptor.Encrypt generates a fresh
// 32-byte DEK per object, newEncryptReader generates a fresh random base nonce
// per call, and chunk indices are sequential within one object, so the XOR
// produces a unique nonce per chunk. A re-uploaded plaintext gets a different
// DEK and base nonce. A PutObject encrypts once and every retry replays that
// same ciphertext, so no nonce is ever reused under a DEK with other bytes.
//
// Relaxing the DEK-per-object invariant would require replacing this with
// random per-chunk nonces or a NIST-compliant counter mode.

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
	if cs <= 0 || cs > config.MaxEncryptionChunkSize {
		return 0, nil, fmt.Errorf("invalid chunk size in header: %d", cs)
	}
	nonce := make([]byte, NonceSize)
	copy(nonce, hdr[9:21])

	return cs, nonce, nil
}

// SameEncryptionOperation reports whether an envelope header read off a
// backend was produced by the same encryption operation as the stored key
// blob packed by PackKeyData. Every encryption run draws a fresh base nonce,
// so a matching nonce means the blob's DEK encrypted these bytes. This
// establishes identity, not authenticity: forged header bytes still fail the
// AEAD tag on the first real read.
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
// on reading from the start. Callers use it to check that a row's encrypted
// flag agrees with the stored bytes. A short stream is reported as not an
// envelope; a read error is returned with a reader that replays what arrived.
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
