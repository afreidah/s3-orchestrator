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

package encryption

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/util/ioutilx"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Encryptor provides encrypt and decrypt operations using envelope encryption
// with a pluggable KeyProvider for DEK wrapping. Per-stream buffers come from
// bufPool, which keeps the hot path allocation-free once the pool is warm.
type Encryptor struct {
	provider  KeyProvider
	chunkSize int
	bufPool   *sync.Pool
}

// EncryptResult holds the output of an encryption operation, including the
// ciphertext stream and metadata to store in the database. BaseNonce lets a
// later range read decrypt without fetching the header. The plaintext DEK is
// kept off this struct because it flows through the write path.
type EncryptResult struct {
	Body           io.Reader // ciphertext stream: header plus encrypted chunks
	CiphertextSize int64
	WrappedDEK     []byte // the encrypted DEK to store on the row
	KeyID          string // which master key wrapped it
	BaseNonce      []byte // the 12-byte nonce embedded in the header
}

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// NewEncryptor creates an Encryptor with the given key provider and chunk
// size. The chunk size must be positive. Config validation enforces stricter
// bounds (4KB-1MB, power of 2); this guard catches programming errors.
func NewEncryptor(provider KeyProvider, chunkSize int) (*Encryptor, error) {
	if chunkSize <= 0 {
		return nil, fmt.Errorf("encryption chunk size must be positive, got %d", chunkSize)
	}
	return &Encryptor{
		provider:  provider,
		chunkSize: chunkSize,
		bufPool:   &sync.Pool{New: func() any { return newChunkBuffers(chunkSize) }},
	}, nil
}

// ForOneObject returns an encryptor for a read that decrypts one object's ranges
// many times. It unwraps the object's key once and reuses it, so it must serve
// only that object and be dropped when the read ends.
func (e *Encryptor) ForOneObject() *Encryptor {
	one := *e
	one.provider = &unwrapOnce{KeyProvider: e.provider}
	return &one
}

// Provider returns the underlying KeyProvider.
func (e *Encryptor) Provider() KeyProvider { return e.provider }

// Shutdown releases what the key provider holds, such as a Vault provider's
// token renewal. The DI container calls it once the process stops, and only
// for an Encryptor that was built.
func (e *Encryptor) Shutdown() { CloseProvider(e.provider) }

// -------------------------------------------------------------------------
// ENCRYPT
// -------------------------------------------------------------------------

// Encrypt generates a random DEK, wraps it with the KeyProvider, and returns
// a streaming ciphertext reader along with encryption metadata. The object's
// ETag is not computed here: the write path digests the plaintext during the
// pass that buffers it, before this layer sees the bytes.
func (e *Encryptor) Encrypt(ctx context.Context, body io.Reader, plaintextSize int64) (*EncryptResult, error) {
	dek, wrappedDEK, keyID, err := e.GenerateAndWrapDEK(ctx)
	if err != nil {
		return nil, err
	}
	return e.EncryptWithDEK(body, plaintextSize, dek, wrappedDEK, keyID)
}

// EncryptWithDEK encrypts under an already-wrapped DEK, skipping WrapDEK. Each
// call draws a fresh base nonce, so reusing the DEK keeps (key, nonce) unique.
// Buffers come from the pool and go back when the reader reaches io.EOF.
func (e *Encryptor) EncryptWithDEK(body io.Reader, plaintextSize int64, dek, wrappedDEK []byte, keyID string) (*EncryptResult, error) {
	bufs, release := e.borrowChunkBuffers(e.chunkSize)
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
	}, nil
}

// GenerateAndWrapDEK produces a fresh 256-bit DEK and wraps it via the
// KeyProvider, returning the unwrapped DEK along with the wrapped form
// and key ID, for callers that need a DEK before encrypting, such as a
// multipart upload that reuses one DEK across its parts.
func (e *Encryptor) GenerateAndWrapDEK(ctx context.Context) (dek, wrappedDEK []byte, keyID string, err error) {
	dek = make([]byte, 32)
	_, _ = rand.Read(dek)
	wrappedDEK, keyID, err = e.provider.WrapDEK(ctx, dek)
	if err != nil {
		return nil, nil, "", fmt.Errorf("wrap DEK: %w", err)
	}
	return dek, wrappedDEK, keyID, nil
}

// borrowChunkBuffers returns buffers that hold a chunk of chunkSize and the
// release that puts them back in the pool. Pooled buffers hold any chunk up to
// the configured size; an object written under a larger setting gets buffers
// of its own and a nil release.
func (e *Encryptor) borrowChunkBuffers(chunkSize int) (*chunkBuffers, func()) {
	if chunkSize > e.chunkSize {
		return newChunkBuffers(chunkSize), nil
	}
	bufs := e.bufPool.Get().(*chunkBuffers)
	return bufs, func() { e.bufPool.Put(bufs) }
}

// -------------------------------------------------------------------------
// DECRYPT
// -------------------------------------------------------------------------

// openDecryptReader starts decrypting chunks of chunkSize at startChunk over
// borrowed buffers, returning them if the reader cannot be built.
func (e *Encryptor) openDecryptReader(body io.Reader, dek, baseNonce []byte, chunkSize int, startChunk uint64) (*decryptReader, error) {
	bufs, release := e.borrowChunkBuffers(chunkSize)
	dr, err := newDecryptReader(body, dek, baseNonce, chunkSize, startChunk, bufs, release)
	if err != nil && release != nil {
		release()
	}
	return dr, err
}

// DecryptStored decrypts a stored object body from its packed key blob
// (baseNonce||wrappedDEK, the form held in object_locations.encryption_key).
// When rng is nil the body starts at the header and the reader yields the full
// plaintext, fullSize long; otherwise the body holds only the chunks
// CiphertextRange named and the reader yields the requested range. A malformed
// key blob is reported as ErrInvalidKeyData. It records EncryptionOpsTotal or
// EncryptionErrorsTotal, so callers need no decrypt telemetry of their own.
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

// decryptStored is DecryptStored without the telemetry. A whole-object read
// takes its chunk size and base nonce from the header the body starts with.
func (e *Encryptor) decryptStored(ctx context.Context, body io.Reader, packedKey []byte, keyID string, fullSize int64, rng *RangeResult) (io.Reader, int64, error) {
	baseNonce, wrappedDEK, err := UnpackKeyData(packedKey)
	if err != nil {
		return nil, 0, err
	}
	dek, err := e.provider.UnwrapDEK(ctx, wrappedDEK, keyID)
	if err != nil {
		return nil, 0, fmt.Errorf("unwrap DEK: %w", err)
	}

	if rng == nil {
		chunkSize, headerNonce, err := parseHeader(body)
		if err != nil {
			return nil, 0, fmt.Errorf("parse header: %w", err)
		}
		dr, err := e.openDecryptReader(body, dek, headerNonce, chunkSize, 0)
		if err != nil {
			return nil, 0, err
		}
		return dr, fullSize, nil
	}

	dr, err := e.openDecryptReader(body, dek, baseNonce, rng.ChunkSize, rng.StartChunk)
	if err != nil {
		return nil, 0, fmt.Errorf("decrypt reader: %w", err)
	}
	reader, err := ioutilx.Window(dr, rng.SliceStart, rng.SliceLen)
	if err != nil {
		return nil, 0, fmt.Errorf("skip to range start: %w", err)
	}
	return reader, rng.SliceLen, nil
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
