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
	"github.com/afreidah/s3-orchestrator/internal/util/materialize"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// encryptOp labels the encrypt side of the encryption metrics.
const encryptOp = "encrypt"

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

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

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// materializeEncrypted encrypts plaintext into a materialized body of its own
// and reports its size and the envelope that describes those bytes. The caller
// layers integrity fields (e.g. ContentHash) onto the returned StoredForm.
//
// The ciphertext is materialized rather than streamed so one encrypt pass
// serves however many uploads a write makes of it: a second pass would draw a
// fresh base nonce, and copies of a key that differ byte for byte break the
// assumption replication reads them under, that the bytes on one backend are
// the bytes the source row describes.
//
// plaintext must be a reader positioned at offset 0.
func materializeEncrypted(
	ctx context.Context,
	enc *encryption.Encryptor,
	plaintext io.Reader,
	plaintextSize int64,
) (*materialize.Body, int64, *core.StoredForm, error) {
	result, err := enc.Encrypt(ctx, plaintext, plaintextSize)
	if err != nil {
		telemetry.EncryptionErrorsTotal.WithLabelValues(encryptOp, "encrypt_failed").Inc()
		return nil, 0, nil, fmt.Errorf("encrypt: %w", err)
	}
	telemetry.EncryptionOpsTotal.WithLabelValues(encryptOp).Inc()
	body, err := materialize.New(withStreamMetric(result.Body, encryptOp), result.CiphertextSize)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("buffer ciphertext: %w", err)
	}
	return body, result.CiphertextSize, &core.StoredForm{
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
