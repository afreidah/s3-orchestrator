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
// checks a copy it has just created. Both answer the same question through one
// verify, and must answer it identically: a digest computed over a different
// layer would read as corruption on one path and as verified on the other.
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
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/etag"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/storedform"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// storedHasher turns a stored copy back into the plaintext the client wrote and
// hashes it. Encryptor and Codec are optional and are what the stored form has
// to be undone through; a copy recorded as encrypted or compressed cannot be
// hashed without the matching one, and surfaces as unreadable rather than
// corrupt. The encryption-flag-mismatch counter names the caller by the
// workload it hashes under.
type storedHasher struct {
	ops       Ops
	encryptor *encryption.Encryptor
	codec     storedform.Decompressor
}

// newStoredHasher builds a hasher over the fleet operations and whichever of
// the encryptor and codec the deployment has.
func newStoredHasher(ops Ops, enc *encryption.Encryptor, codec storedform.Decompressor) *storedHasher {
	return &storedHasher{ops: ops, encryptor: enc, codec: codec}
}

// storedDigests are the two digests one read of a stored copy produces: the
// SHA-256 integrity verifies against, and the MD5 that is the object's ETag.
// Both come off the same pass, so the ETag of an object whose stored bytes are
// not the client's costs no extra egress to learn.
type storedDigests struct {
	SHA256 string
	MD5    string
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// hashStored reads a copy from its backend and returns the digests of the
// bytes the client wrote. Records the API call and egress against the
// backend's usage quota.
func (h *storedHasher) hashStored(ctx context.Context, loc *core.ObjectLocation) (storedDigests, error) {
	// A row that contradicts itself about encryption cannot produce a
	// meaningful plaintext hash, so it is rejected before a backend read is
	// spent on it.
	if err := core.ValidateEncryptionMetadata(loc); err != nil {
		countFlagMismatch(ctx)
		return storedDigests{}, err
	}

	be, err := h.ops.GetBackend(loc.BackendName)
	if err != nil {
		return storedDigests{}, err
	}

	// The copy's own path. A row carrying none keeps its bytes at the object's
	// key.
	result, cancel, err := h.ops.Get(ctx, loc.BackendName, be, core.StoragePath(loc.ObjectKey, loc.StorageKey), "")
	if err != nil {
		return storedDigests{}, fmt.Errorf("get object: %w", err)
	}
	defer cancel()
	defer result.Body.Close()

	h.ops.Acct().Egress(ctx, s3op.GetObject, loc.BackendName, result.Size)

	// Opening checks the stored bytes against what the row claims first.
	// Hashing an envelope as if it were plaintext writes a ciphertext digest
	// into content_hash, which makes the mismatch look verified forever and
	// turns any later repair of the flag into a false integrity failure.
	reader, closeDecoded, err := storedform.Open(ctx, h.encryptor, h.codec, result.Body, &loc.StoredForm)
	if err != nil {
		if errors.Is(err, core.ErrEncryptionFlagMismatch) {
			countFlagMismatch(ctx)
		}
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

// countFlagMismatch counts a copy whose row contradicts its bytes about
// encryption, under the workload that found it.
func countFlagMismatch(ctx context.Context) {
	telemetry.EncryptionFlagMismatchTotal.WithLabelValues(string(audit.WorkloadOf(ctx))).Inc()
}

// -------------------------------------------------------------------------
// VERIFICATION
// -------------------------------------------------------------------------

// CopyOutcome is what verifying one copy established. The zero value is
// deliberately not a valid outcome, so an unset field cannot read as verified.
type CopyOutcome int

// The outcomes a verification reports. Only a mismatch says anything is wrong
// with the copy: the others say it could not be checked, and why.
const (
	CopyVerified CopyOutcome = iota + 1
	CopyMismatch
	CopyUnreadable
	CopyNotHashed
	CopyDeclined
)

// The operations integrity checks are counted under, alongside the read
// path's "read".
const (
	integrityOpScrub     = "scrub"
	integrityOpReplicate = "replicate"
)

// String names the outcome for logs and test failures. It is not the wire
// value: how a verdict reads to an operator is the admin transport's business,
// and it words each one there.
func (o CopyOutcome) String() string {
	switch o {
	case CopyVerified:
		return "verified"
	case CopyMismatch:
		return "mismatch"
	case CopyUnreadable:
		return "unreadable"
	case CopyNotHashed:
		return "not hashed"
	case CopyDeclined:
		return "declined"
	default:
		return "unknown"
	}
}

// verify reads the copy at read and compares its plaintext digest against
// want's content hash, counting the check under op. admit decides whether
// the read's egress may be spent. The read and the check that judges it are
// usually one row; a new replica is judged by the source it was copied from.
// A mismatch publishes IntegrityCorruptionFound, and acting on it is the
// caller's business.
func (h *storedHasher) verify(ctx context.Context, read, want *core.ObjectLocation, op string, admit func(backend string, egress int64) bool) CopyOutcome {
	if !want.Form().Has(core.StoredHashed) {
		return CopyNotHashed
	}
	log := logfmt.Logger(string(audit.WorkloadOf(ctx)))
	if !admit(read.BackendName, read.SizeBytes) {
		telemetry.IntegrityUsageDeclinedTotal.Inc()
		log.WarnContext(ctx, "integrity check declined by usage limits",
			"key", read.ObjectKey, "backend", read.BackendName, "size", read.SizeBytes)
		return CopyDeclined
	}
	actual, err := h.hashStored(ctx, read)
	if err != nil {
		log.WarnContext(ctx, "integrity check could not read copy",
			"key", read.ObjectKey, "backend", read.BackendName, "error", err)
		return CopyUnreadable
	}
	telemetry.IntegrityChecksTotal.WithLabelValues(op).Inc()
	if actual.SHA256 == want.ContentHash {
		return CopyVerified
	}
	telemetry.IntegrityErrorsTotal.WithLabelValues(op).Inc()
	log.ErrorContext(ctx, "integrity check failed",
		"key", read.ObjectKey, "backend", read.BackendName,
		"expected_hash", want.ContentHash, "actual_hash", actual.SHA256)
	event.Publish(event.IntegrityCorruptionFound, read.ObjectKey, map[string]any{
		"key":           read.ObjectKey,
		"backend":       read.BackendName,
		"expected_hash": want.ContentHash,
		"actual_hash":   actual.SHA256,
		"size_bytes":    read.SizeBytes,
	})
	return CopyMismatch
}

// admitsRead reports whether a check may read egress bytes from a backend:
// the whole limit, as a client read gets, with no scrub reserve held back.
func (h *storedHasher) admitsRead(backend string, egress int64) bool {
	return h.ops.Acct().Allow(backend, s3op.GetObject, egress, 0)
}
