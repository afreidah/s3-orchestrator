// -------------------------------------------------------------------------------
// Stored Form - Content Hash Verification
//
// Author: Alex Freidah
//
// A copy's content hash covers the bytes the client wrote, so checking stored
// bytes against it means undoing the stored form first. Digest does that for
// every path that hashes a whole stored object, and ContentMatches is the one
// judgment of a digest against the hash: a read, a scrub, a new replica and a
// rewrite count the check and report a mismatch the same way.
// -------------------------------------------------------------------------------

package storedform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/bufpool"
)

// ErrContentMismatch reports bytes that do not hash to the content hash their
// row records.
var ErrContentMismatch = errors.New("bytes do not match the content hash")

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Digest opens body as form describes and writes the bytes the client wrote
// to each sink, typically the hashers a caller compares. Reading the stored
// bytes as they are would hash ciphertext or an encoding, never the bytes the
// content hash covers.
func Digest(ctx context.Context, enc *encryption.Encryptor, codec Decompressor, body io.Reader, form *core.StoredForm, sinks ...io.Writer) error {
	r, closeDecoded, err := Open(ctx, enc, codec, body, form)
	if err != nil {
		return err
	}
	defer closeDecoded()
	if _, err := bufpool.Copy(io.MultiWriter(sinks...), r); err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	return nil
}

// ContentMatches counts an integrity check of loc under op and reports whether
// actual, the digest of the bytes read, is the expected content hash. A
// mismatch is counted, logged and published as IntegrityCorruptionFound;
// acting on it is the caller's business.
func ContentMatches(ctx context.Context, log *slog.Logger, op string, loc *core.ObjectLocation, expected, actual string) bool {
	telemetry.IntegrityChecksTotal.WithLabelValues(op).Inc()
	if actual == expected {
		return true
	}
	telemetry.IntegrityErrorsTotal.WithLabelValues(op).Inc()
	log.ErrorContext(ctx, "integrity check failed",
		"key", loc.ObjectKey, "backend", loc.BackendName,
		"expected_hash", expected, "actual_hash", actual)
	event.Publish(event.IntegrityCorruptionFound, loc.ObjectKey, map[string]any{
		"key":           loc.ObjectKey,
		"backend":       loc.BackendName,
		"expected_hash": expected,
		"actual_hash":   actual,
		"size_bytes":    loc.SizeBytes,
	})
	return false
}

// Verify checks that out reads back, under the form it now carries, as the
// bytes src's content hash covers, counting the check under op. A reform of
// src's bytes that does not is ErrContentMismatch. A source with no content
// hash has nothing to check against.
func (l *Layers) Verify(ctx context.Context, log *slog.Logger, op string, src *core.ObjectLocation, out *Reformed) error {
	if src.ContentHash == "" {
		return nil
	}
	h := sha256.New()
	if err := Digest(ctx, l.Encryptor, l.Codec, out.Body.Reader(), out.Form, h); err != nil {
		return fmt.Errorf("read back reformed object: %w", err)
	}
	if !ContentMatches(ctx, log, op, src, src.ContentHash, hex.EncodeToString(h.Sum(nil))) {
		return fmt.Errorf("copy on %s: %w", src.BackendName, ErrContentMismatch)
	}
	return nil
}
