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
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// ScrubberStore is the narrow persistence surface the scrubber needs:
// integrity row reads/writes. Declared locally so the worker does not
// pull in the full MetadataStore.
type ScrubberStore interface {
	core.IntegrityStore
}

// Scrubber periodically verifies stored object integrity by reading objects
// from backends, computing their SHA-256 hash, and comparing against the
// stored content hash. Also supports backfilling hashes for objects that
// were written before integrity was enabled.
type Scrubber struct {
	log       *slog.Logger
	deps      ScrubberOps
	placement Placement
	store     ScrubberStore
	encryptor *encryption.Encryptor
	cfg       syncutil.AtomicConfig[config.IntegrityConfig]
}

// ScrubberDeps groups the scrubber's constructor dependencies. Encryptor
// is optional (nil when encryption is disabled).
type ScrubberDeps struct {
	Ops       ScrubberOps
	Placement Placement
	Store     ScrubberStore
	Encryptor *encryption.Encryptor
}

// NewScrubber creates a Scrubber with the given dependencies.
func NewScrubber(deps ScrubberDeps) *Scrubber {
	must.NotNil("Ops", deps.Ops)
	must.NotNil("Placement", deps.Placement)
	must.NotNil("Store", deps.Store)
	return &Scrubber{deps: deps.Ops, placement: deps.Placement, store: deps.Store, encryptor: deps.Encryptor, log: slog.Default().With(logfmt.Component("scrubber"))}
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
func (s *Scrubber) Scrub(ctx context.Context, batchSize int, observer progress.Observer) (checked, failed int) {
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
		if verified, matched := s.verifyOne(ctx, &locs[i], observer); verified {
			checked++
			telemetry.IntegrityChecksTotal.WithLabelValues("scrub").Inc()
			if !matched {
				failed++
			}
		}
	}

	s.log.InfoContext(ctx, "scrub cycle complete",
		"checked", checked, "failed", failed, "duration", time.Since(start))
	return checked, failed
}

// verifyOne verifies one object's stored hash, bracketing the work with
// observer start/end steps. Returns whether the object was verified (no error)
// and whether its hash matched.
func (s *Scrubber) verifyOne(ctx context.Context, loc *core.ObjectLocation, observer progress.Observer) (verified, matched bool) {
	progress.Track(observer, loc.ObjectKey, func() string {
		match, verifyErr := s.verifyObject(ctx, loc)
		if verifyErr != nil {
			s.log.WarnContext(ctx, "failed to verify object",
				"key", loc.ObjectKey, "backend", loc.BackendName, "error", verifyErr)
			return progress.StatusFailed
		}
		verified = true
		matched = match
		if match {
			return progress.StatusOK
		}
		return "mismatch"
	})
	return verified, matched
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
			s.placement.DeleteOrEnqueue(ctx, be, loc.BackendName, loc.ObjectKey,
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
// objects starting at the given offset. observer, when non-nil, receives a
// start step before each object is hashed and an end step after, carrying the
// per-object outcome and duration. Returns the number of objects processed and
// the next offset for pagination (0 when done).
func (s *Scrubber) Backfill(ctx context.Context, batchSize, offset int, observer progress.Observer) (processed, nextOffset int) {
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
		if s.hashOne(ctx, &locs[i], observer) {
			processed++
		}
	}

	s.log.InfoContext(ctx, "backfill batch complete",
		"processed", processed, "batch_size", len(locs), "duration", time.Since(start))

	// If we got a full batch, there may be more
	if len(locs) == batchSize {
		return processed, offset + batchSize
	}
	return processed, 0
}

// hashOne computes and stores the hash for one object, bracketing the work with
// observer start/end steps. Returns true when the hash was stored.
func (s *Scrubber) hashOne(ctx context.Context, loc *core.ObjectLocation, observer progress.Observer) bool {
	stored := false
	progress.Track(observer, loc.ObjectKey, func() string {
		hash, hashErr := s.readAndHash(ctx, loc)
		if hashErr != nil {
			s.log.WarnContext(ctx, "failed to hash object",
				"key", loc.ObjectKey, "backend", loc.BackendName, "error", hashErr)
			return progress.StatusFailed
		}
		if err := s.store.UpdateContentHash(ctx, loc.ObjectKey, loc.BackendName, hash); err != nil {
			s.log.WarnContext(ctx, "failed to store hash",
				"key", loc.ObjectKey, "backend", loc.BackendName, "error", err)
			return progress.StatusFailed
		}
		stored = true
		return progress.StatusOK
	})
	return stored
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
