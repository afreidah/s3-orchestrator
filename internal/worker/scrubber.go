// -------------------------------------------------------------------------------
// Scrubber - Background Integrity Verification Worker
//
// Author: Alex Freidah
//
// Provides two operations on stored objects:
//
// Scrub reads random objects that have stored SHA-256 hashes and verifies
// their content still matches. Corrupted copies are enqueued for cleanup.
//
// Backfill reads objects that have no stored hash, computes the hash, and
// stores it in the database so future scrub and read-time checks cover them.
//
// Both operations decrypt encrypted objects before hashing so the comparison
// is always against the original plaintext. Each backend read is tracked
// against the backend's usage quota (API calls + egress).
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/audit"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"github.com/afreidah/s3-orchestrator/internal/telemetry"
)

// Scrubber periodically verifies stored object integrity by reading objects
// from backends, computing their SHA-256 hash, and comparing against the
// stored content hash. Also supports backfilling hashes for objects that
// were written before integrity was enabled.
type Scrubber struct {
	ops       Ops
	encryptor *encryption.Encryptor
	cfg       atomic.Pointer[config.IntegrityConfig]
}

// NewScrubber creates a Scrubber with the given Ops and optional encryptor.
func NewScrubber(ops Ops, encryptor *encryption.Encryptor) *Scrubber {
	return &Scrubber{ops: ops, encryptor: encryptor}
}

// SetConfig atomically stores the integrity configuration.
func (s *Scrubber) SetConfig(cfg *config.IntegrityConfig) {
	s.cfg.Store(cfg)
}

// Config returns the current integrity configuration.
func (s *Scrubber) Config() *config.IntegrityConfig {
	return s.cfg.Load()
}

// -------------------------------------------------------------------------
// SCRUB — verify existing hashes
// -------------------------------------------------------------------------

// Scrub verifies a batch of objects with stored content hashes. Returns the
// number of objects checked and the number of hash mismatches found.
func (s *Scrubber) Scrub(ctx context.Context, batchSize int) (checked, failed int) {
	start := time.Now()
	ctx = audit.WithRequestID(ctx, audit.NewID())
	ctx, span := telemetry.StartSpan(ctx, "Scrub")
	defer span.End()

	locs, err := s.ops.Store().GetRandomHashedObjects(ctx, batchSize)
	if err != nil {
		slog.ErrorContext(ctx, "Scrubber: failed to fetch objects", "error", err)
		return 0, 0
	}

	for i := range locs {
		if ctx.Err() != nil {
			break
		}
		match, verifyErr := s.verifyObject(ctx, &locs[i])
		if verifyErr != nil {
			slog.WarnContext(ctx, "Scrubber: failed to verify object",
				"key", locs[i].ObjectKey, "backend", locs[i].BackendName, "error", verifyErr)
			continue
		}
		checked++
		telemetry.IntegrityChecksTotal.WithLabelValues("scrub").Inc()
		if !match {
			failed++
		}
	}

	slog.InfoContext(ctx, "Scrub cycle complete",
		"checked", checked, "failed", failed, "duration", time.Since(start))
	return checked, failed
}

// verifyObject reads a single object, computes its hash, and compares to
// the stored content hash. On mismatch the corrupted copy is enqueued for
// cleanup. Returns true if the hash matches.
func (s *Scrubber) verifyObject(ctx context.Context, loc *store.ObjectLocation) (bool, error) {
	actual, err := s.readAndHash(ctx, loc)
	if err != nil {
		return false, err
	}

	if actual != loc.ContentHash {
		be, _ := s.ops.GetBackend(loc.BackendName)
		slog.ErrorContext(ctx, "Scrubber: integrity check failed",
			"key", loc.ObjectKey, "backend", loc.BackendName,
			"expected_hash", loc.ContentHash, "actual_hash", actual)
		telemetry.IntegrityErrorsTotal.WithLabelValues("scrub").Inc()
		if be != nil {
			s.ops.DeleteOrEnqueue(ctx, be, loc.BackendName, loc.ObjectKey,
				"integrity_scrub_failed", loc.SizeBytes)
		}
		return false, nil
	}

	return true, nil
}

// -------------------------------------------------------------------------
// BACKFILL — compute hashes for objects that don't have one
// -------------------------------------------------------------------------

// Backfill reads objects that have no stored content hash, computes the
// SHA-256 digest, and stores it in the database. Processes up to batchSize
// objects starting at the given offset. Returns the number of objects
// processed and the next offset for pagination (0 when done).
func (s *Scrubber) Backfill(ctx context.Context, batchSize, offset int) (processed, nextOffset int) {
	start := time.Now()
	ctx = audit.WithRequestID(ctx, audit.NewID())
	ctx, span := telemetry.StartSpan(ctx, "Backfill")
	defer span.End()

	locs, err := s.ops.Store().GetObjectsWithoutHash(ctx, batchSize, offset)
	if err != nil {
		slog.ErrorContext(ctx, "Backfill: failed to fetch objects", "error", err)
		return 0, 0
	}

	if len(locs) == 0 {
		return 0, 0
	}

	for i := range locs {
		if ctx.Err() != nil {
			break
		}
		loc := &locs[i]
		hash, hashErr := s.readAndHash(ctx, loc)
		if hashErr != nil {
			slog.WarnContext(ctx, "Backfill: failed to hash object",
				"key", loc.ObjectKey, "backend", loc.BackendName, "error", hashErr)
			continue
		}

		if err := s.ops.Store().UpdateContentHash(ctx, loc.ObjectKey, loc.BackendName, hash); err != nil {
			slog.WarnContext(ctx, "Backfill: failed to store hash",
				"key", loc.ObjectKey, "backend", loc.BackendName, "error", err)
			continue
		}

		processed++
	}

	slog.InfoContext(ctx, "Backfill batch complete",
		"processed", processed, "batch_size", len(locs), "duration", time.Since(start))

	// If we got a full batch, there may be more
	if len(locs) == batchSize {
		return processed, offset + batchSize
	}
	return processed, 0
}

// -------------------------------------------------------------------------
// SHARED — read object from backend, decrypt if needed, compute SHA-256
// -------------------------------------------------------------------------

// readAndHash reads an object from its backend, decrypts if encrypted, and
// returns the SHA-256 hex digest of the plaintext. Records API call and
// egress against the backend's usage quota.
func (s *Scrubber) readAndHash(ctx context.Context, loc *store.ObjectLocation) (string, error) {
	be, err := s.ops.GetBackend(loc.BackendName)
	if err != nil {
		return "", err
	}

	bctx, bcancel := s.ops.WithTimeout(ctx)
	defer bcancel()

	result, err := be.GetObject(bctx, loc.ObjectKey, "")
	if err != nil {
		s.ops.Usage().Record(loc.BackendName, 1, 0, 0)
		return "", fmt.Errorf("get object: %w", err)
	}
	defer result.Body.Close()

	// Record API call + egress
	s.ops.Usage().Record(loc.BackendName, 1, result.Size, 0)

	// Decrypt if the object is encrypted — hash is computed on plaintext
	var reader io.Reader = result.Body
	if loc.Encrypted && s.encryptor != nil {
		_, wrappedDEK, unpackErr := encryption.UnpackKeyData(loc.EncryptionKey)
		if unpackErr != nil {
			return "", fmt.Errorf("unpack key data: %w", unpackErr)
		}
		decrypted, decErr := s.encryptor.Decrypt(ctx, result.Body, wrappedDEK, loc.KeyID)
		if decErr != nil {
			return "", fmt.Errorf("decrypt: %w", decErr)
		}
		reader = decrypted
	}

	// Compute SHA-256 of the (plaintext) body
	h := sha256.New()
	if _, err := io.Copy(h, reader); err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
