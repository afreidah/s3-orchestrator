// -------------------------------------------------------------------------------
// Encryption Helpers - Shared Encrypt/Decrypt Pipeline Stages
//
// Author: Alex Freidah
//
// Standalone helpers for encrypting request bodies and decrypting response
// bodies. Used by Manager (PutObject, GetObject) and MultipartManager
// (UploadPart, CompleteMultipartUpload) to avoid duplicating the encrypt ->
// build-metadata -> record-telemetry pattern.
// -------------------------------------------------------------------------------

package object

import (
	"context"
	"errors"
	"fmt"
	"io"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/ioutilx"
)

// streamMetricReader counts non-EOF errors surfaced by the wrapped
// reader's Read method. Used to surface mid-stream encryption /
// decryption failures (e.g. backend network reset during a copy)
// through the same s3o_encryption_errors_total counter the
// constructor-time errors flow through.
type streamMetricReader struct {
	io.Reader
	op string
}

// Read forwards to the wrapped reader and increments the encryption
// errors counter when a non-EOF error surfaces.
func (m *streamMetricReader) Read(p []byte) (int, error) {
	n, err := m.Reader.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		telemetry.EncryptionErrorsTotal.WithLabelValues(m.op, "stream_failed").Inc()
	}
	return n, err
}

// withStreamMetric wraps r so non-EOF Read errors increment the metric.
// The wrapper is a thin io.Reader; callers retain responsibility for
// any io.Closer they supplied alongside r.
func withStreamMetric(r io.Reader, op string) io.Reader {
	return &streamMetricReader{Reader: r, op: op}
}

// putEncryptState carries the cached DEK across PutObject failover attempts
// so retries reuse the wrapped DEK instead of paying another KeyProvider
// round-trip. The zero value means "no DEK cached yet  -  wrap on first call".
type putEncryptState struct {
	dek, wrappedDEK []byte
	keyID           string
}

// encryptForPut prepares an upload body for PutObject. The first call wraps
// a fresh DEK via the KeyProvider and stores it in state; subsequent calls
// reuse the cached DEK with a new base nonce, so retry storms do not
// hammer the KeyProvider. Returns the ciphertext stream, its size, and the
// stored form of those bytes. The caller layers integrity fields
// (e.g. ContentHash) onto the returned StoredForm.
//
// plaintext must be a reader positioned at offset 0; callers replaying
// across failover attempts pass a fresh reader (or a rewound seeker) per
// call so the Encryptor sees the full payload each time.
func encryptForPut(
	ctx context.Context,
	enc *encryption.Encryptor,
	plaintext io.Reader,
	plaintextSize int64,
	state *putEncryptState,
) (io.Reader, int64, *core.StoredForm, error) {
	var (
		result *encryption.EncryptResult
		err    error
	)
	if state.dek == nil {
		result, err = enc.Encrypt(ctx, plaintext, plaintextSize)
		if err == nil {
			state.dek = result.RawDEK()
			state.wrappedDEK = result.WrappedDEK
			state.keyID = result.KeyID
		}
	} else {
		result, err = enc.EncryptWithDEK(plaintext, plaintextSize, state.dek, state.wrappedDEK, state.keyID)
	}
	if err != nil {
		telemetry.EncryptionErrorsTotal.WithLabelValues("encrypt", "encrypt_failed").Inc()
		return nil, 0, nil, fmt.Errorf("encrypt: %w", err)
	}
	telemetry.EncryptionOpsTotal.WithLabelValues("encrypt").Inc()
	return withStreamMetric(result.Body, "encrypt"), result.CiphertextSize, &core.StoredForm{
		Encrypted:     true,
		EncryptionKey: encryption.PackKeyData(result.BaseNonce, result.WrappedDEK),
		KeyID:         result.KeyID,
		PlaintextSize: plaintextSize,
	}, nil
}

// decryptResponse decrypts a GetObject response body in place. Handles both
// full reads and range requests. Mutates r.Body, r.Size, and r.ContentRange.
// Decrypt ops/errors telemetry is owned by Encryptor.DecryptStored; this only
// adds the mid-stream metric wrapper. The caller must close r.Body on error.
func decryptResponse(ctx context.Context, enc *encryption.Encryptor, r *s3be.GetObjectResult, loc *core.ObjectLocation, rng *encryption.RangeResult, ptStart, ptEnd int64) error {
	op := "decrypt"
	if rng != nil {
		op = "decrypt_range"
	}

	plainReader, plainLen, err := enc.DecryptStored(ctx, r.Body, loc.EncryptionKey, loc.KeyID, loc.PlaintextSize, rng)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}

	r.Body = ioutilx.ReadCloser(withStreamMetric(plainReader, op), r.Body)
	r.Size = plainLen
	if rng != nil {
		r.ContentRange = fmt.Sprintf("bytes %d-%d/%d", ptStart, ptEnd, loc.PlaintextSize)
	}
	return nil
}
