// -------------------------------------------------------------------------------
// Encryptor - Server-Side Envelope Encryption
//
// Author: Alex Freidah
//
// High-level API for encrypting and decrypting S3 objects using envelope
// encryption with chunked AES-256-GCM. Each object gets a random 256-bit DEK
// that is wrapped by the configured KeyProvider before storage. Supports full
// object encryption, full decryption, and range-based decryption for HTTP
// Range requests.
// -------------------------------------------------------------------------------

// Package encryption provides envelope encryption for object payloads.
// It frames plaintext into chunked AES-GCM segments addressable by byte
// range and supports pluggable key providers (config-embedded, file,
// and Vault transit).
package encryption

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Encryptor provides encrypt and decrypt operations using envelope encryption
// with a pluggable KeyProvider for DEK wrapping.
//
// Per-stream byte buffers (plaintext staging, framed ciphertext, nonce,
// header) come from bufPool. Reusing the same buffer set across streams
// keeps the encrypt + decrypt hot path allocation-free once the pool is
// warm; under load the orchestrator was previously dominated by these
// per-stream allocations.
type Encryptor struct {
	provider  KeyProvider
	chunkSize int
	bufPool   sync.Pool
}

// EncryptResult holds the output of an encryption operation, including the
// ciphertext stream and metadata to store in the database.
type EncryptResult struct {
	// Body is the ciphertext stream (header + encrypted chunks).
	Body io.Reader

	// CiphertextSize is the total size of the ciphertext output.
	CiphertextSize int64

	// WrappedDEK is the encrypted DEK to store in the database.
	WrappedDEK []byte

	// KeyID identifies which master key wrapped the DEK.
	KeyID string

	// BaseNonce is the 12-byte nonce embedded in the header, needed for
	// range-based decryption without fetching the header from the backend.
	BaseNonce []byte

	// rawDEK is the plaintext DEK used for encryption. Reachable only via
	// the RawDEK accessor so it does not surface in struct printers,
	// marshalers, or log lines that walk exported fields.
	rawDEK []byte
}

// RawDEK returns the plaintext DEK so the caller can reuse it on retry via
// EncryptWithDEK, avoiding an extra KeyProvider round-trip. Callers must
// not persist or log the returned bytes.
func (r *EncryptResult) RawDEK() []byte { return r.rawDEK }

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// NewEncryptor creates an Encryptor with the given key provider and chunk
// size. The chunk size must be positive. Config validation enforces stricter
// bounds (4KB-1MB, power of 2); this guard catches programming errors.
//
// The buffer pool is seeded with chunkBuffers sized to chunkSize so every
// borrowing reader gets a buffer set that fits a worst-case full chunk
// without growth.
func NewEncryptor(provider KeyProvider, chunkSize int) (*Encryptor, error) {
	if chunkSize <= 0 {
		return nil, fmt.Errorf("encryption chunk size must be positive, got %d", chunkSize)
	}
	e := &Encryptor{
		provider:  provider,
		chunkSize: chunkSize,
	}
	e.bufPool.New = func() any { return newChunkBuffers(chunkSize) }
	return e, nil
}

// ChunkSize returns the configured plaintext chunk size.
func (e *Encryptor) ChunkSize() int { return e.chunkSize }

// Provider returns the underlying KeyProvider.
func (e *Encryptor) Provider() KeyProvider { return e.provider }

// -------------------------------------------------------------------------
// ENCRYPT
// -------------------------------------------------------------------------

// Encrypt generates a random DEK, wraps it with the KeyProvider, and returns
// a streaming ciphertext reader along with encryption metadata. The plaintext
// is read from body and its MD5 digest is computed on the fly for ETag
// generation.
func (e *Encryptor) Encrypt(ctx context.Context, body io.Reader, plaintextSize int64) (*EncryptResult, error) {
	// Generate random DEK
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return nil, fmt.Errorf("generate DEK: %w", err)
	}
	wrappedDEK, keyID, err := e.provider.WrapDEK(ctx, dek)
	if err != nil {
		return nil, fmt.Errorf("wrap DEK: %w", err)
	}
	return e.assembleEncryptResult(body, plaintextSize, dek, wrappedDEK, keyID)
}

// EncryptWithDEK encrypts using a previously wrapped DEK, skipping the
// KeyProvider.WrapDEK call. A fresh base nonce is generated per call, so
// the ciphertext is unique even though the same DEK is reused. This is
// safe because AES-GCM nonce uniqueness is per (key, nonce) pair  -  see
// the SAFETY INVARIANT comment in chunk.go.
//
// Intended for write failover retries where the Vault round-trip for key
// wrapping has already been paid on the first attempt.
func (e *Encryptor) EncryptWithDEK(body io.Reader, plaintextSize int64, dek, wrappedDEK []byte, keyID string) (*EncryptResult, error) {
	return e.assembleEncryptResult(body, plaintextSize, dek, wrappedDEK, keyID)
}

// GenerateAndWrapDEK produces a fresh 256-bit DEK and wraps it via the
// KeyProvider, returning the unwrapped DEK along with the wrapped form
// and key ID. Used by callers that need a wrapped DEK ahead of any
// actual encryption work - notably CreateMultipartUpload, which
// persists the wrapped DEK on the upload row so every subsequent
// UploadPart can reuse it without making its own WrapDEK round-trip.
func (e *Encryptor) GenerateAndWrapDEK(ctx context.Context) (dek, wrappedDEK []byte, keyID string, err error) {
	dek = make([]byte, 32)
	if _, err = rand.Read(dek); err != nil {
		return nil, nil, "", fmt.Errorf("generate DEK: %w", err)
	}
	wrappedDEK, keyID, err = e.provider.WrapDEK(ctx, dek)
	if err != nil {
		return nil, nil, "", fmt.Errorf("wrap DEK: %w", err)
	}
	return dek, wrappedDEK, keyID, nil
}

// assembleEncryptResult builds the streaming ciphertext reader and the
// EncryptResult shared by Encrypt and EncryptWithDEK. The two callers
// differ only in how they obtain (dek, wrappedDEK, keyID); everything
// downstream is identical. Buffers come from the per-Encryptor pool
// and are returned automatically when the reader reaches io.EOF.
func (e *Encryptor) assembleEncryptResult(body io.Reader, plaintextSize int64, dek, wrappedDEK []byte, keyID string) (*EncryptResult, error) {
	bufs, release := e.borrowChunkBuffers()
	encReader, err := newEncryptReader(body, dek, e.chunkSize, bufs, release)
	if err != nil {
		release()
		return nil, fmt.Errorf("encrypt reader: %w", err)
	}
	return &EncryptResult{
		Body:           encReader,
		CiphertextSize: ciphertextSizeExact(plaintextSize, e.chunkSize),
		WrappedDEK:     wrappedDEK,
		KeyID:          keyID,
		BaseNonce:      encReader.baseNonce,
		rawDEK:         dek,
	}, nil
}

// borrowChunkBuffers fetches a chunkBuffers from the pool and returns
// the release closure that puts it back. The reader fires release on
// io.EOF; callers also fire it on a construction error path.
func (e *Encryptor) borrowChunkBuffers() (*chunkBuffers, func()) {
	bufs := e.bufPool.Get().(*chunkBuffers)
	return bufs, func() { e.bufPool.Put(bufs) }
}

// -------------------------------------------------------------------------
// DECRYPT
// -------------------------------------------------------------------------

// Decrypt unwraps the DEK and returns a streaming plaintext reader for the
// entire encrypted object. The body must start at the beginning of the
// ciphertext (including the header). Buffers come from the per-Encryptor
// pool and are returned automatically when the reader reaches io.EOF.
func (e *Encryptor) Decrypt(ctx context.Context, body io.Reader, wrappedDEK []byte, keyID string) (io.Reader, error) {
	dek, err := e.provider.UnwrapDEK(ctx, wrappedDEK, keyID)
	if err != nil {
		return nil, fmt.Errorf("unwrap DEK: %w", err)
	}

	chunkSize, baseNonce, err := ParseHeader(body)
	if err != nil {
		return nil, fmt.Errorf("parse header: %w", err)
	}

	bufs, release := e.borrowChunkBuffers()
	dr, err := newDecryptReader(body, dek, baseNonce, chunkSize, 0, bufs, release)
	if err != nil {
		release()
		return nil, err
	}
	return dr, nil
}

// DecryptRange unwraps the DEK and returns a streaming plaintext reader for
// a range of the encrypted object. The body should contain only the
// ciphertext chunks identified by CiphertextRange (no header). Returns the
// plaintext reader and the number of plaintext bytes it will produce.
func (e *Encryptor) DecryptRange(ctx context.Context, body io.Reader, wrappedDEK []byte, keyID string, rng *RangeResult, baseNonce []byte) (io.Reader, int64, error) {
	dek, err := e.provider.UnwrapDEK(ctx, wrappedDEK, keyID)
	if err != nil {
		return nil, 0, fmt.Errorf("unwrap DEK: %w", err)
	}

	bufs, release := e.borrowChunkBuffers()
	dr, err := newDecryptReader(body, dek, baseNonce, e.chunkSize, rng.StartChunk, bufs, release)
	if err != nil {
		release()
		return nil, 0, fmt.Errorf("decrypt reader: %w", err)
	}

	// Skip bytes within the first chunk to reach the requested offset, then
	// limit output to the requested length.
	var reader io.Reader = dr
	if rng.SliceStart > 0 {
		if _, err := io.CopyN(io.Discard, dr, rng.SliceStart); err != nil {
			return nil, 0, fmt.Errorf("skip to range start: %w", err)
		}
	}
	reader = io.LimitReader(reader, rng.SliceLen)

	return reader, rng.SliceLen, nil
}

// DecryptStored decrypts a stored object body from its packed key blob
// (baseNonce||wrappedDEK, the form held in object_locations.encryption_key).
// When rng is nil it returns a reader for the full plaintext and fullSize as the
// length; otherwise it returns the requested plaintext range and that range's
// length. It centralizes the "stored key blob -> decrypt call" mapping (only the
// range path needs the base nonce) so callers do not repeat the unpack-and-branch.
// A malformed key blob is reported as ErrInvalidKeyData.
//
// It is the single decrypt entry point: on successful reader construction it
// increments EncryptionOpsTotal, and on a construction failure it increments
// EncryptionErrorsTotal, so callers do not maintain their own decrypt telemetry.
func (e *Encryptor) DecryptStored(ctx context.Context, body io.Reader, packedKey []byte, keyID string, fullSize int64, rng *RangeResult) (io.Reader, int64, error) {
	op := "decrypt"
	if rng != nil {
		op = "decrypt_range"
	}

	reader, n, err := e.decryptStored(ctx, body, packedKey, keyID, fullSize, rng)
	if err != nil {
		reason := "decrypt_failed"
		if errors.Is(err, ErrInvalidKeyData) {
			reason = "unpack_failed"
		}
		telemetry.EncryptionErrorsTotal.WithLabelValues(op, reason).Inc()
		return nil, 0, err
	}
	telemetry.EncryptionOpsTotal.WithLabelValues(op).Inc()
	return reader, n, nil
}

// decryptStored performs the unpack-and-branch without telemetry; DecryptStored
// wraps it to keep the op-label and counter bookkeeping in one place.
func (e *Encryptor) decryptStored(ctx context.Context, body io.Reader, packedKey []byte, keyID string, fullSize int64, rng *RangeResult) (io.Reader, int64, error) {
	baseNonce, wrappedDEK, err := UnpackKeyData(packedKey)
	if err != nil {
		return nil, 0, err
	}
	if rng != nil {
		return e.DecryptRange(ctx, body, wrappedDEK, keyID, rng, baseNonce)
	}
	plain, err := e.Decrypt(ctx, body, wrappedDEK, keyID)
	if err != nil {
		return nil, 0, err
	}
	return plain, fullSize, nil
}

// -------------------------------------------------------------------------
// CIPHERTEXT SIZE
// -------------------------------------------------------------------------

// CiphertextSize returns the total ciphertext size for a given plaintext
// size using this Encryptor's chunk size.
func (e *Encryptor) CiphertextSize(plaintextSize int64) int64 {
	return ciphertextSizeExact(plaintextSize, e.chunkSize)
}

// -------------------------------------------------------------------------
// KEY DATA PACKING
// -------------------------------------------------------------------------

// PackKeyData packs the base nonce and wrapped DEK into a single byte slice
// for storage in the database. Format: baseNonce (12B) || wrappedDEK.
func PackKeyData(baseNonce, wrappedDEK []byte) []byte {
	buf := make([]byte, len(baseNonce)+len(wrappedDEK))
	copy(buf, baseNonce)
	copy(buf[len(baseNonce):], wrappedDEK)
	return buf
}

// ErrInvalidKeyData is returned when stored key data cannot be unpacked into a
// base nonce and wrapped DEK (e.g. it is shorter than the nonce).
var ErrInvalidKeyData = errors.New("encryption key data too short")

// UnpackKeyData splits stored key data into the base nonce and wrapped DEK.
func UnpackKeyData(data []byte) (baseNonce, wrappedDEK []byte, err error) {
	if len(data) <= NonceSize {
		return nil, nil, fmt.Errorf("%w: %d bytes", ErrInvalidKeyData, len(data))
	}
	return data[:NonceSize], data[NonceSize:], nil
}
