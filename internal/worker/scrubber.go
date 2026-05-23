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
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// Scrubber periodically verifies stored object integrity by reading objects
// from backends, computing their SHA-256 hash, and comparing against the
// stored content hash. Also supports backfilling hashes for objects that
// were written before integrity was enabled.
type Scrubber struct {
	log *slog.Logger
	deps      ScrubberOps
	store     core.MetadataStore
	encryptor *encryption.Encryptor
	cfg       syncutil.AtomicConfig[config.IntegrityConfig]
}

// NewScrubber creates a Scrubber with the given dependencies and optional encryptor.
func NewScrubber(deps ScrubberOps, store core.MetadataStore, encryptor *encryption.Encryptor) *Scrubber {
	must.NotNil("deps", deps)
	must.NotNil("store", store)
	return &Scrubber{deps: deps, store: store, encryptor: encryptor, log: slog.Default().With(logfmt.Component("scrubber"))}
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
// SCRUB  -  verify existing hashes
// -------------------------------------------------------------------------

// Scrub verifies a batch of objects with stored content hashes. Returns the
// number of objects checked and the number of hash mismatches found.
func (s *Scrubber) Scrub(ctx context.Context, batchSize int) (checked, failed int) {
	start := time.Now()
	ctx = audit.WithRequestID(ctx, audit.NewID())
	ctx, span := telemetry.StartSpan(ctx, "Scrub")
	defer span.End()

	locs, err := s.store.GetRandomHashedObjects(ctx, batchSize)
	if err != nil {
		s.log.ErrorContext(ctx, "failed to fetch objects", "error", err)
		return 0, 0
	}

	for i := range locs {
		if ctx.Err() != nil {
			break
		}
		match, verifyErr := s.verifyObject(ctx, &locs[i])
		if verifyErr != nil {
			s.log.WarnContext(ctx, "failed to verify object",
				"key", locs[i].ObjectKey, "backend", locs[i].BackendName, "error", verifyErr)
			continue
		}
		checked++
		telemetry.IntegrityChecksTotal.WithLabelValues("scrub").Inc()
		if !match {
			failed++
		}
	}

	s.log.InfoContext(ctx, "scrub cycle complete",
		"checked", checked, "failed", failed, "duration", time.Since(start))
	return checked, failed
}

// verifyObject reads a single object, computes its hash, and compares to
// the stored content hash. On mismatch the corrupted copy is enqueued for
// cleanup. Returns true if the hash matches.
func (s *Scrubber) verifyObject(ctx context.Context, loc *core.ObjectLocation) (bool, error) {
	actual, err := s.readAndHash(ctx, loc)
	if err != nil {
		return false, err
	}

	if actual != loc.ContentHash {
		be, _ := s.deps.GetBackend(loc.BackendName)
		s.log.ErrorContext(ctx, "integrity check failed",
			"key", loc.ObjectKey, "backend", loc.BackendName,
			"expected_hash", loc.ContentHash, "actual_hash", actual)
		telemetry.IntegrityErrorsTotal.WithLabelValues("scrub").Inc()
		if be != nil {
			s.deps.DeleteOrEnqueue(ctx, be, loc.BackendName, loc.ObjectKey,
				"integrity_scrub_failed", loc.SizeBytes)
		}
		return false, nil
	}

	return true, nil
}

// -------------------------------------------------------------------------
// BACKFILL  -  compute hashes for objects that don't have one
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

	locs, err := s.store.GetObjectsWithoutHash(ctx, batchSize, offset)
	if err != nil {
		s.log.ErrorContext(ctx, "failed to fetch objects", "error", err)
		return 0, 0
	}

	if len(locs) == 0 {
		return 0, 0
	}

	s.log.InfoContext(ctx, "backfill batch starting",
		"objects", len(locs), "offset", offset)

	for i := range locs {
		if ctx.Err() != nil {
			break
		}
		loc := &locs[i]
		hash, hashErr := s.readAndHash(ctx, loc)
		if hashErr != nil {
			s.log.WarnContext(ctx, "failed to hash object",
				"key", loc.ObjectKey, "backend", loc.BackendName, "error", hashErr)
			continue
		}

		if err := s.store.UpdateContentHash(ctx, loc.ObjectKey, loc.BackendName, hash); err != nil {
			s.log.WarnContext(ctx, "failed to store hash",
				"key", loc.ObjectKey, "backend", loc.BackendName, "error", err)
			continue
		}

		processed++
	}

	s.log.InfoContext(ctx, "backfill batch complete",
		"processed", processed, "batch_size", len(locs), "duration", time.Since(start))

	// If we got a full batch, there may be more
	if len(locs) == batchSize {
		return processed, offset + batchSize
	}
	return processed, 0
}

// -------------------------------------------------------------------------
// SHARED  -  read object from backend, decrypt if needed, compute SHA-256
// -------------------------------------------------------------------------

// readAndHash reads an object from its backend, decrypts if encrypted, and
// returns the SHA-256 hex digest of the plaintext. Records API call and
// egress against the backend's usage quota.
func (s *Scrubber) readAndHash(ctx context.Context, loc *core.ObjectLocation) (string, error) {
	be, err := s.deps.GetBackend(loc.BackendName)
	if err != nil {
		return "", err
	}

	bctx, bcancel := s.deps.WithTimeout(ctx)
	defer bcancel()

	result, err := be.GetObject(bctx, loc.ObjectKey, "")
	if err != nil {
		s.deps.Acct().APICall(loc.BackendName)
		return "", fmt.Errorf("get object: %w", err)
	}
	defer result.Body.Close()

	s.deps.Acct().Egress(loc.BackendName, result.Size)

	// Decrypt if the object is encrypted  -  hash is computed on plaintext
	var reader io.Reader = result.Body
	if loc.Encrypted && s.encryptor != nil {
		_, wrappedDEK, unpackErr := encryption.UnpackKeyData(loc.EncryptionKey)
		if unpackErr != nil {
			return "", fmt.Errorf("unpack key data: %w", unpackErr)
		}
		decrypted, decErr := s.encryptor.Decrypt(ctx, result.Body, wrappedDEK, loc.KeyID)
		if decErr != nil {
			telemetry.EncryptionErrorsTotal.WithLabelValues("decrypt", "decrypt_failed").Inc()
			return "", fmt.Errorf("decrypt: %w", decErr)
		}
		telemetry.EncryptionOpsTotal.WithLabelValues("decrypt").Inc()
		reader = decrypted
	}

	// Compute SHA-256 of the (plaintext) body
	h := sha256.New()
	if _, err := io.Copy(h, reader); err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
