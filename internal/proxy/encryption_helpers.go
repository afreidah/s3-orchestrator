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
	"context"
	"fmt"
	"io"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store"
)

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
