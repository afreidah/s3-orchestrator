// -------------------------------------------------------------------------------
// Stored-Form Hashing - Shared Plaintext Digest of a Stored Copy
//
// Author: Alex Freidah
//
// Reads one copy from its backend, undoes whatever the ledger row says was done
// to it - decrypt, then decompress - and returns the SHA-256 of the bytes the
// client wrote. That digest is what object_locations.content_hash holds, so it
// is the only form a stored copy can be compared against.
//
// Shared by the scrubber, which sweeps the fleet, and the replicator, which
// checks a copy it has just created. Both answer the same question and must
// answer it identically: a digest computed over a different layer would read as
// corruption on one path and as verified on the other.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/etag"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// errNoCodec reports a compressed copy this orchestrator cannot decode, which
// is a copy it cannot judge. It surfaces as an unreadable copy rather than a
// failed one, because treating "cannot read" as "corrupt" deletes objects that
// were never damaged.
var errNoCodec = errors.New("object is compressed but no codec is configured")

// StoredReader is the narrow surface hashing a stored copy needs: find the
// backend, read the object, and charge the read to that backend's quota.
// Satisfied by both Ops and ScrubberOps.
type StoredReader interface {
	DataMover
	RecorderProvider
}

// storedHasher turns a stored copy back into the plaintext the client wrote and
// hashes it. Encryptor and Codec are optional and are what the stored form has
// to be undone through; a copy recorded as encrypted or compressed cannot be
// hashed without the matching one.
type storedHasher struct {
	ops       StoredReader
	encryptor *encryption.Encryptor
	codec     StreamDecompressor
	source    string
}

// newStoredHasher builds a hasher that attributes its metrics to source, which
// names the caller in the encryption-flag-mismatch counter.
func newStoredHasher(ops StoredReader, enc *encryption.Encryptor, codec StreamDecompressor, source string) *storedHasher {
	return &storedHasher{ops: ops, encryptor: enc, codec: codec, source: source}
}

// storedDigests are the two digests one read of a stored copy produces: the
// SHA-256 integrity verifies against, and the MD5 that is the object's ETag.
// Both come off the same pass, so the ETag of an object whose stored bytes are
// not the client's costs no extra egress to learn.
type storedDigests struct {
	SHA256 string
	MD5    string
}

// hashStored reads a copy from its backend and returns the digests of the
// bytes the client wrote. Records the API call and egress against the
// backend's usage quota.
func (h *storedHasher) hashStored(ctx context.Context, loc *core.ObjectLocation) (storedDigests, error) {
	// A row that contradicts itself about encryption cannot produce a
	// meaningful plaintext hash, so it is rejected before a backend read is
	// spent on it.
	if err := core.ValidateEncryptionMetadata(loc); err != nil {
		telemetry.EncryptionFlagMismatchTotal.WithLabelValues(h.source).Inc()
		return storedDigests{}, err
	}

	be, err := h.ops.GetBackend(loc.BackendName)
	if err != nil {
		return storedDigests{}, err
	}

	result, cancel, err := h.ops.GetWithTimeout(ctx, be, loc.ObjectKey, "")
	if err != nil {
		h.ops.Acct().APICall(s3op.GetObject, loc.BackendName)
		return storedDigests{}, fmt.Errorf("get object: %w", err)
	}
	defer cancel()
	defer result.Body.Close()

	h.ops.Acct().Egress(s3op.GetObject, loc.BackendName, result.Size)

	// Check the stored bytes against what the row claims before hashing.
	// Hashing an envelope as if it were plaintext writes a ciphertext digest
	// into content_hash, which makes the mismatch look verified forever and
	// turns any later repair of the flag into a false integrity failure.
	isEnvelope, body, err := encryption.PeekEnvelope(result.Body)
	if err != nil {
		return storedDigests{}, fmt.Errorf("inspect object header: %w", err)
	}
	if isEnvelope != loc.Encrypted {
		telemetry.EncryptionFlagMismatchTotal.WithLabelValues(h.source).Inc()
		return storedDigests{}, fmt.Errorf("%w: row says encrypted=%t but stored bytes say encrypted=%t",
			core.ErrEncryptionFlagMismatch, loc.Encrypted, isEnvelope)
	}

	reader, closeDecoded, err := h.decode(ctx, body, loc)
	if err != nil {
		return storedDigests{}, err
	}
	defer closeDecoded()

	hasher := sha256.New()
	etagHasher := etag.NewHasher()
	if _, err := io.Copy(io.MultiWriter(hasher, etagHasher), reader); err != nil {
		return storedDigests{}, fmt.Errorf("read body: %w", err)
	}
	return storedDigests{SHA256: hex.EncodeToString(hasher.Sum(nil)), MD5: etag.Hex(etagHasher)}, nil
}

// decode undoes the stored form in the order it was applied, so the hash covers
// the bytes the client wrote: decrypt, then decompress. Hashing either layer as
// if it were plaintext writes a digest of the wrong bytes into content_hash,
// which every later verification then reads as corruption. The returned closer
// is always safe to call.
func (h *storedHasher) decode(ctx context.Context, body io.Reader, loc *core.ObjectLocation) (io.Reader, func(), error) {
	noop := func() {
		// Nothing to close on the paths that wrap no decoder.
	}

	reader := body
	if loc.Encrypted && h.encryptor != nil {
		decrypted, _, err := h.encryptor.DecryptStored(ctx, body, loc.EncryptionKey, loc.KeyID, loc.PlaintextSize, nil)
		if err != nil {
			return nil, noop, fmt.Errorf("decrypt: %w", err)
		}
		reader = decrypted
	}

	if loc.CompressionAlgorithm == "" {
		return reader, noop, nil
	}
	if h.codec == nil {
		return nil, noop, errNoCodec
	}
	// Decoded front to back rather than through the seek table: the whole
	// object is read anyway, and a streaming decode avoids buffering it
	// locally just to have something seekable.
	decoded, err := h.codec.DecompressStream(reader)
	if err != nil {
		return nil, noop, fmt.Errorf("decompress: %w", err)
	}
	return decoded, func() { _ = decoded.Close() }, nil
}
