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
func (s *Scrubber) Scrub(ctx context.Context, batchSize int, observer progress.Observer) WorkSummary {
	ctx = audit.WithRequestID(ctx, audit.NewID())
	ctx, span := telemetry.StartSpan(ctx, "Scrub")
	defer span.End()

	locs, err := s.store.GetRandomHashedObjects(ctx, batchSize)
	if err != nil {
		s.log.ErrorContext(ctx, "failed to fetch objects", "error", err)
		return WorkSummary{}
	}

	// Scrub stays sequential (Concurrency 1): each item reads and hashes a full
	// object body, so a wider window can hammer the backends. Concurrency is a
	// parameter now, so raising it later is a config change, not a rewrite.
	runner := BatchRunner[core.ObjectLocation]{
		Name:        "scrub",
		Log:         s.log,
		Concurrency: 1,
		Observer:    observer,
		Key:         func(l core.ObjectLocation) string { return l.ObjectKey },
	}
	return runner.Run(ctx, locs, func(ctx context.Context, loc core.ObjectLocation) ItemResult {
		res := s.verifyOne(ctx, &loc)
		// "checked" = an object we actually verified (matched or mismatched); a
		// verify error is skipped, not checked.
		if res.Outcome != ItemSkipped {
			telemetry.IntegrityChecksTotal.WithLabelValues("scrub").Inc()
		}
		return res
	})
}

// verifyOne verifies one object's stored hash and classifies the result for the
// batch tally: a matched hash succeeds, a mismatch fails, and a verify error is
// skipped (not counted as checked). The returned Status feeds the progress
// stream the BatchRunner brackets each item with.
func (s *Scrubber) verifyOne(ctx context.Context, loc *core.ObjectLocation) ItemResult {
	match, verifyErr := s.verifyObject(ctx, loc)
	if verifyErr != nil {
		s.log.WarnContext(ctx, "failed to verify object",
			"key", loc.ObjectKey, "backend", loc.BackendName, "error", verifyErr)
		return ItemResult{Outcome: ItemSkipped, Status: progress.StatusFailed}
	}
	if match {
		return ItemResult{Outcome: ItemSucceeded, Status: progress.StatusOK}
	}
	return ItemResult{Outcome: ItemFailed, Status: "mismatch"}
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
// per-object outcome and duration. Returns the cycle summary and the next
// offset for pagination (0 when done).
func (s *Scrubber) Backfill(ctx context.Context, batchSize, offset int, observer progress.Observer) (WorkSummary, int) {
	ctx = audit.WithRequestID(ctx, audit.NewID())
	ctx, span := telemetry.StartSpan(ctx, "Backfill")
	defer span.End()

	locs, err := s.store.GetObjectsWithoutHash(ctx, batchSize, offset)
	if err != nil {
		s.log.ErrorContext(ctx, "failed to fetch objects", "error", err)
		return WorkSummary{}, 0
	}

	if len(locs) == 0 {
		return WorkSummary{}, 0
	}

	s.log.InfoContext(ctx, "backfill batch starting",
		"objects", len(locs), "offset", offset)

	// Sequential (Concurrency 1) like Scrub: each item reads and hashes a full
	// object body.
	runner := BatchRunner[core.ObjectLocation]{
		Name:        "backfill",
		Log:         s.log,
		Concurrency: 1,
		Observer:    observer,
		Key:         func(l core.ObjectLocation) string { return l.ObjectKey },
	}
	sum := runner.Run(ctx, locs, func(ctx context.Context, loc core.ObjectLocation) ItemResult {
		return s.hashOne(ctx, &loc)
	})

	// A full batch means there may be more rows to page through.
	nextOffset := 0
	if len(locs) == batchSize {
		nextOffset = offset + batchSize
	}
	return sum, nextOffset
}

// hashOne computes and stores the hash for one object, returning the outcome
// for the batch tally and a status for the progress stream.
func (s *Scrubber) hashOne(ctx context.Context, loc *core.ObjectLocation) ItemResult {
	hash, hashErr := s.readAndHash(ctx, loc)
	if hashErr != nil {
		s.log.WarnContext(ctx, "failed to hash object",
			"key", loc.ObjectKey, "backend", loc.BackendName, "error", hashErr)
		return ItemResult{Outcome: ItemFailed, Status: progress.StatusFailed}
	}
	if err := s.store.UpdateContentHash(ctx, loc.ObjectKey, loc.BackendName, hash); err != nil {
		s.log.WarnContext(ctx, "failed to store hash",
			"key", loc.ObjectKey, "backend", loc.BackendName, "error", err)
		return ItemResult{Outcome: ItemFailed, Status: progress.StatusFailed}
	}
	return ItemResult{Outcome: ItemSucceeded, Status: progress.StatusOK}
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

	result, cancel, err := s.deps.GetWithTimeout(ctx, be, loc.ObjectKey, "")
	if err != nil {
		s.deps.Acct().APICall(loc.BackendName)
		return "", fmt.Errorf("get object: %w", err)
	}
	defer cancel()
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
