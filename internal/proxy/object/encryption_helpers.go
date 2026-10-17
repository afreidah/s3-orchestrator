// -------------------------------------------------------------------------------
// Encryption Helpers - Response Decryption
//
// Author: Alex Freidah
//
// Decrypts GetObject response bodies, whole or ranged, for the read path. The
// encrypt side of a write goes through the shared stored-form stages.
// -------------------------------------------------------------------------------

package object

import (
	"context"
	"fmt"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/storedform"
	"github.com/afreidah/s3-orchestrator/internal/util/ioutilx"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

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

	r.Body = ioutilx.ReadCloser(storedform.WithStreamMetric(plainReader, op), r.Body)
	r.Size = plainLen
	if rng != nil {
		r.ContentRange = fmt.Sprintf("bytes %d-%d/%d", ptStart, ptEnd, loc.PlaintextSize)
	}
	return nil
}
