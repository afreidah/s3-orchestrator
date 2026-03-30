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
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Encryptor provides encrypt and decrypt operations using envelope encryption
// with a pluggable KeyProvider for DEK wrapping.
type Encryptor struct {
	provider  KeyProvider
	chunkSize int
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

	// PlaintextMD5 is the hex-encoded MD5 digest of the plaintext, used as
	// the client-facing ETag.
	PlaintextMD5 string

	// RawDEK is the plaintext DEK used for encryption. Exposed so callers
	// can reuse it for retry attempts via EncryptWithDEK without an
	// additional KeyProvider round-trip. Must not be persisted or logged.
	RawDEK []byte
}

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// NewEncryptor creates an Encryptor with the given key provider and chunk
// size. The chunk size must be positive. Config validation enforces stricter
// bounds (4KB–1MB, power of 2); this guard catches programming errors.
func NewEncryptor(provider KeyProvider, chunkSize int) (*Encryptor, error) {
	if chunkSize <= 0 {
		return nil, fmt.Errorf("encryption chunk size must be positive, got %d", chunkSize)
	}
	return &Encryptor{
		provider:  provider,
		chunkSize: chunkSize,
	}, nil
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

	// Wrap DEK with master key
	wrappedDEK, keyID, err := e.provider.WrapDEK(ctx, dek)
	if err != nil {
		return nil, fmt.Errorf("wrap DEK: %w", err)
	}

	// Tee the plaintext through an MD5 hash for ETag computation
	md5Hash := md5.New()
	teeBody := io.TeeReader(body, md5Hash)

	// Create streaming encrypt reader
	encReader, err := newEncryptReader(teeBody, dek, e.chunkSize)
	if err != nil {
		return nil, fmt.Errorf("encrypt reader: %w", err)
	}

	ctSize := ciphertextSizeExact(plaintextSize, e.chunkSize)

	fr := &md5FinalizingReader{reader: encReader, hash: md5Hash}
	result := &EncryptResult{
		Body:           fr,
		CiphertextSize: ctSize,
		WrappedDEK:     wrappedDEK,
		KeyID:          keyID,
		BaseNonce:      encReader.baseNonce,
		RawDEK:         dek,
	}
	fr.setResult(result)

	return result, nil
}

// EncryptWithDEK encrypts using a previously wrapped DEK, skipping the
// KeyProvider.WrapDEK call. A fresh base nonce is generated per call, so
// the ciphertext is unique even though the same DEK is reused. This is
// safe because AES-GCM nonce uniqueness is per (key, nonce) pair — see
// the SAFETY INVARIANT comment in chunk.go.
//
// Intended for write failover retries where the Vault round-trip for key
// wrapping has already been paid on the first attempt.
func (e *Encryptor) EncryptWithDEK(body io.Reader, plaintextSize int64, dek, wrappedDEK []byte, keyID string) (*EncryptResult, error) {
	md5Hash := md5.New()
	teeBody := io.TeeReader(body, md5Hash)

	encReader, err := newEncryptReader(teeBody, dek, e.chunkSize)
	if err != nil {
		return nil, fmt.Errorf("encrypt reader: %w", err)
	}

	ctSize := ciphertextSizeExact(plaintextSize, e.chunkSize)

	fr := &md5FinalizingReader{reader: encReader, hash: md5Hash}
	result := &EncryptResult{
		Body:           fr,
		CiphertextSize: ctSize,
		WrappedDEK:     wrappedDEK,
		KeyID:          keyID,
		BaseNonce:      encReader.baseNonce,
		RawDEK:         dek,
	}
	fr.setResult(result)

	return result, nil
}

// -------------------------------------------------------------------------
// DECRYPT
// -------------------------------------------------------------------------

// Decrypt unwraps the DEK and returns a streaming plaintext reader for the
// entire encrypted object. The body must start at the beginning of the
// ciphertext (including the header).
func (e *Encryptor) Decrypt(ctx context.Context, body io.Reader, wrappedDEK []byte, keyID string) (io.Reader, error) {
	dek, err := e.provider.UnwrapDEK(ctx, wrappedDEK, keyID)
	if err != nil {
		return nil, fmt.Errorf("unwrap DEK: %w", err)
	}

	chunkSize, baseNonce, err := ParseHeader(body)
	if err != nil {
		return nil, fmt.Errorf("parse header: %w", err)
	}

	return newDecryptReader(body, dek, baseNonce, chunkSize, 0)
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

	dr, err := newDecryptReader(body, dek, baseNonce, e.chunkSize, rng.StartChunk)
	if err != nil {
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

// -------------------------------------------------------------------------
// CIPHERTEXT SIZE
// -------------------------------------------------------------------------

// CiphertextSize returns the total ciphertext size for a given plaintext
// size using this Encryptor's chunk size.
func (e *Encryptor) CiphertextSize(plaintextSize int64) int64 {
	return ciphertextSizeExact(plaintextSize, e.chunkSize)
}

// -------------------------------------------------------------------------
// MD5 FINALIZING READER
// -------------------------------------------------------------------------

// md5FinalizingReader wraps an encrypt reader and captures the MD5 digest
// after the plaintext has been fully consumed. The digest is available via
// the PlaintextMD5 field on EncryptResult after the reader returns io.EOF.
type md5FinalizingReader struct {
	reader *encryptReader
	hash   hash.Hash
	result *EncryptResult
}

// Read delegates to the encrypt reader. When EOF is reached, the MD5 digest
// is finalized and stored in the associated EncryptResult.
func (r *md5FinalizingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err == io.EOF && r.result != nil && r.result.PlaintextMD5 == "" {
		r.result.PlaintextMD5 = hex.EncodeToString(r.hash.Sum(nil))
	}
	return n, err
}

// setResult links the finalizing reader to its EncryptResult so the MD5
// digest can be stored when encryption completes.
func (r *md5FinalizingReader) setResult(res *EncryptResult) {
	r.result = res
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

// UnpackKeyData splits stored key data into the base nonce and wrapped DEK.
func UnpackKeyData(data []byte) (baseNonce, wrappedDEK []byte, err error) {
	if len(data) <= NonceSize {
		return nil, nil, fmt.Errorf("encryption key data too short: %d bytes", len(data))
	}
	return data[:NonceSize], data[NonceSize:], nil
}
