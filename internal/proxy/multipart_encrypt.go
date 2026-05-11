// -------------------------------------------------------------------------------
// Multipart Manager - Encryption and DEK Helpers
//
// Author: Alex Freidah
//
// Per-upload DEK unwrap with TTL caching, plus the body-encryption helpers
// shared between UploadPart and CompleteMultipartUpload's assembly path.
// The upload-level DEK is wrapped once at CreateMultipartUpload time and
// reused for every part and the assembled object, so the per-upload Unwrap
// round-trip is paid at most once per process per upload.
// -------------------------------------------------------------------------------

package proxy

import (
	"context"
	"fmt"
	"io"

	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// unwrapUploadDEK returns the unwrapped DEK for a multipart upload,
// caching the result for the lifetime of the upload so subsequent
// UploadParts on this instance do not re-issue the KeyProvider round-
// trip. Returns the unwrapped DEK and the wrapped form (for write-path
// metadata that needs the wrapped value).
func (mp *MultipartManager) unwrapUploadDEK(ctx context.Context, mu *core.MultipartUpload) (dek, wrappedDEK []byte, baseNonce []byte, err error) {
	if !mu.Encrypted || len(mu.EncryptionKey) == 0 {
		return nil, nil, nil, fmt.Errorf("upload %s carries no encryption metadata", mu.UploadID)
	}
	baseNonce, wrappedDEK, err = encryption.UnpackKeyData(mu.EncryptionKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("unpack upload encryption metadata: %w", err)
	}
	if cached, ok := mp.dekCache.Get(mu.UploadID); ok {
		return cached, wrappedDEK, baseNonce, nil
	}
	unwrapped, err := mp.encryptor.Provider().UnwrapDEK(ctx, wrappedDEK, mu.KeyID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("unwrap upload DEK: %w", err)
	}
	mp.dekCache.Set(mu.UploadID, unwrapped)
	return unwrapped, wrappedDEK, baseNonce, nil
}

// forgetUploadDEK drops a cached unwrapped DEK so the upload's DEK
// stops occupying memory once the upload has reached a terminal state
// (Complete/Abort/expiry).
func (mp *MultipartManager) forgetUploadDEK(uploadID string) {
	mp.dekCache.Delete(uploadID)
}

// encryptWithUploadDEK wraps body in EncryptWithDEK using the
// upload-level DEK from mu and returns the ciphertext reader, its
// ciphertext size, and the EncryptionMeta the caller should persist on
// the resulting part or object row. The caller is responsible for
// deciding whether to encrypt at all (the upload row may have been
// created unencrypted, or the encryptor may be unconfigured) and for
// wrapping the returned error with a call-site-specific context.
// Counters are incremented here so every successful encrypt and every
// failure path is observable from one place.
func (mp *MultipartManager) encryptWithUploadDEK(ctx context.Context, mu *core.MultipartUpload, body io.Reader, size int64) (io.Reader, int64, *core.EncryptionMeta, error) {
	dek, wrappedDEK, _, err := mp.unwrapUploadDEK(ctx, mu)
	if err != nil {
		return nil, 0, nil, err
	}
	result, err := mp.encryptor.EncryptWithDEK(body, size, dek, wrappedDEK, mu.KeyID)
	if err != nil {
		telemetry.EncryptionErrorsTotal.WithLabelValues("encrypt", "encrypt_failed").Inc()
		return nil, 0, nil, err
	}
	telemetry.EncryptionOpsTotal.WithLabelValues("encrypt").Inc()
	return result.Body, result.CiphertextSize, &core.EncryptionMeta{
		Encrypted:     true,
		EncryptionKey: encryption.PackKeyData(result.BaseNonce, result.WrappedDEK),
		KeyID:         result.KeyID,
		PlaintextSize: size,
	}, nil
}

// prepareUploadPartBody returns the body, size, and encryption metadata
// the backend PUT should use for one part. When the upload row is flagged
// as encrypted and the manager has an encryptor configured, the per-part
// body is wrapped with EncryptWithDEK using the upload-level DEK from the
// dekCache (zero KeyProvider round-trips after the first unwrap). The
// AES-GCM (key, nonce) uniqueness invariant holds across every part
// because EncryptWithDEK generates a fresh per-part base nonce internally.
// When encryption is disabled or the upload was created unencrypted, the
// inputs are returned unchanged with a nil EncryptionMeta.
func (mp *MultipartManager) prepareUploadPartBody(ctx context.Context, mu *core.MultipartUpload, body io.Reader, size int64) (io.Reader, int64, *core.EncryptionMeta, error) {
	if mp.encryptor == nil || !mu.Encrypted {
		return body, size, nil, nil
	}
	out, ciphertextSize, enc, err := mp.encryptWithUploadDEK(ctx, mu, body, size)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("encrypt part: %w", err)
	}
	return out, ciphertextSize, enc, nil
}
