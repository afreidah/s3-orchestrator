// -------------------------------------------------------------------------------
// Encryption Helpers - Shared Encrypt/Decrypt Pipeline Stages
//
// Author: Alex Freidah
//
// Standalone helpers for encrypting request bodies and decrypting response
// bodies. Used by ObjectManager (PutObject, GetObject) and MultipartManager
// (UploadPart, CompleteMultipartUpload) to avoid duplicating the encrypt →
// build-metadata → record-telemetry pattern.
// -------------------------------------------------------------------------------

package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store"
)

// putEncryptState carries the cached DEK across PutObject failover attempts
// so retries reuse the wrapped DEK instead of paying another KeyProvider
// round-trip. The zero value means "no DEK cached yet — wrap on first call".
type putEncryptState struct {
	dek, wrappedDEK []byte
	keyID           string
}

// encryptForPut prepares an upload body for PutObject. The first call wraps
// a fresh DEK via the KeyProvider and stores it in state; subsequent calls
// reuse the cached DEK with a new base nonce, so retry storms do not
// hammer the KeyProvider. Returns the ciphertext stream, its size, and the
// storage-side encryption metadata. The caller layers integrity fields
// (e.g. ContentHash) onto the returned EncryptionMeta.
func encryptForPut(
	ctx context.Context,
	enc *encryption.Encryptor,
	plaintext []byte,
	plaintextSize int64,
	state *putEncryptState,
) (io.Reader, int64, *store.EncryptionMeta, error) {
	var (
		result *encryption.EncryptResult
		err    error
	)
	if state.dek == nil {
		result, err = enc.Encrypt(ctx, bytes.NewReader(plaintext), plaintextSize)
		if err == nil {
			state.dek = result.RawDEK()
			state.wrappedDEK = result.WrappedDEK
			state.keyID = result.KeyID
		}
	} else {
		result, err = enc.EncryptWithDEK(bytes.NewReader(plaintext), plaintextSize, state.dek, state.wrappedDEK, state.keyID)
	}
	if err != nil {
		telemetry.EncryptionErrorsTotal.WithLabelValues("encrypt", "encrypt_failed").Inc()
		return nil, 0, nil, fmt.Errorf("encrypt: %w", err)
	}
	telemetry.EncryptionOpsTotal.WithLabelValues("encrypt").Inc()
	return result.Body, result.CiphertextSize, &store.EncryptionMeta{
		Encrypted:     true,
		EncryptionKey: encryption.PackKeyData(result.BaseNonce, result.WrappedDEK),
		KeyID:         result.KeyID,
		PlaintextSize: plaintextSize,
	}, nil
}

// encryptBody encrypts a request body for upload. Returns the encrypted body,
// ciphertext size, and encryption metadata for the store. Used by UploadPart
// and CompleteMultipartUpload. PutObject uses its own inline path because it
// caches the DEK across retry attempts.
func encryptBody(ctx context.Context, enc *encryption.Encryptor, body io.Reader, plaintextSize int64) (io.Reader, int64, *store.EncryptionMeta, error) {
	result, err := enc.Encrypt(ctx, body, plaintextSize)
	if err != nil {
		telemetry.EncryptionErrorsTotal.WithLabelValues("encrypt", "encrypt_failed").Inc()
		return nil, 0, nil, fmt.Errorf("encrypt: %w", err)
	}
	telemetry.EncryptionOpsTotal.WithLabelValues("encrypt").Inc()

	meta := &store.EncryptionMeta{
		Encrypted:     true,
		EncryptionKey: encryption.PackKeyData(result.BaseNonce, result.WrappedDEK),
		KeyID:         result.KeyID,
		PlaintextSize: plaintextSize,
	}
	return result.Body, result.CiphertextSize, meta, nil
}

// decryptResponse decrypts a GetObject response body in place. Handles both
// full reads and range requests. Mutates r.Body, r.Size, and r.ContentRange.
// The caller must close r.Body on error.
func decryptResponse(ctx context.Context, enc *encryption.Encryptor, r *s3be.GetObjectResult, loc *store.ObjectLocation, rng *encryption.RangeResult, ptStart, ptEnd int64) error {
	baseNonce, wrappedDEK, err := encryption.UnpackKeyData(loc.EncryptionKey)
	if err != nil {
		telemetry.EncryptionErrorsTotal.WithLabelValues("decrypt", "unpack_failed").Inc()
		return fmt.Errorf("unpack key data: %w", err)
	}

	if rng != nil {
		plainReader, plainLen, decErr := enc.DecryptRange(ctx, r.Body, wrappedDEK, loc.KeyID, rng, baseNonce)
		if decErr != nil {
			telemetry.EncryptionErrorsTotal.WithLabelValues("decrypt_range", "decrypt_failed").Inc()
			return fmt.Errorf("decrypt range: %w", decErr)
		}
		telemetry.EncryptionOpsTotal.WithLabelValues("decrypt_range").Inc()
		r.Body = wrapReader(plainReader, r.Body)
		r.Size = plainLen
		r.ContentRange = fmt.Sprintf("bytes %d-%d/%d", ptStart, ptEnd, loc.PlaintextSize)
	} else {
		decrypted, decErr := enc.Decrypt(ctx, r.Body, wrappedDEK, loc.KeyID)
		if decErr != nil {
			telemetry.EncryptionErrorsTotal.WithLabelValues("decrypt", "decrypt_failed").Inc()
			return fmt.Errorf("decrypt: %w", decErr)
		}
		telemetry.EncryptionOpsTotal.WithLabelValues("decrypt").Inc()
		r.Body = wrapReader(decrypted, r.Body)
		r.Size = loc.PlaintextSize
	}
	return nil
}
