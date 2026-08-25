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
// Both operations undo the stored form before hashing - decrypt, then
// decompress - so the comparison is always against the bytes the client wrote.
// That work lives in storedhash.go, shared with the replicator, and tracks each
// backend read against the backend's usage quota (API calls + egress).
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"fmt"
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
// integrity row reads/writes, removal of a location whose bytes failed
// verification, and the copies of one key for an on-demand verification.
// Declared locally so the worker does not pull in the full MetadataStore.
type ScrubberStore interface {
	core.IntegrityStore
	DeleteObjectLocation(ctx context.Context, key, backendName string) error
	GetAllObjectLocations(ctx context.Context, key string) ([]core.ObjectLocation, error)
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
	hasher    *storedHasher
	cfg       syncutil.AtomicConfig[config.IntegrityConfig]
}

// ScrubberDeps groups the scrubber's constructor dependencies. Encryptor and
// Codec are optional, and are what the stored form has to be undone through
// before hashing; a copy recorded as encrypted or compressed cannot be verified
// without the matching one.
type ScrubberDeps struct {
	Ops       ScrubberOps
	Placement Placement
	Store     ScrubberStore
	Encryptor *encryption.Encryptor
	Codec     StreamDecompressor
}

// NewScrubber creates a Scrubber with the given dependencies.
func NewScrubber(deps ScrubberDeps) *Scrubber {
	must.NotNil("Ops", deps.Ops)
	must.NotNil("Placement", deps.Placement)
	must.NotNil("Store", deps.Store)
	return &Scrubber{
		deps:      deps.Ops,
		placement: deps.Placement,
		store:     deps.Store,
		hasher:    newStoredHasher(deps.Ops, deps.Encryptor, deps.Codec, "scrubber"),
		log:       slog.Default().With(logfmt.Component("scrubber")),
	}
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
// CopyVerification is the outcome of verifying one copy of one key.
//
// Outcome distinguishes the three answers that matter: the bytes matched the
// stored hash, they did not, or the copy could not be read at all. A copy with
// no stored hash reports NotHashed rather than success, since there was nothing
// to compare against and reporting it as verified would be a lie.
type CopyVerification struct {
	Backend string
	Outcome CopyOutcome
}

// CopyOutcome is what verifying one copy established. The zero value is
// deliberately not a valid outcome, so an unset field cannot read as verified.
type CopyOutcome int

// Outcomes reported by ScrubKey.
const (
	CopyVerified CopyOutcome = iota + 1
	CopyMismatch
	CopyUnreadable
	CopyNotHashed
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
	default:
		return "unknown"
	}
}

// ScrubKey verifies every copy of one key immediately and reports each
// separately.
//
// This is the question an operator has when something looks wrong - a restore
// failed, a backend threw errors - and the sweep cannot answer it: ordered by
// least-recently-verified, reaching a specific key can take days.
//
// It deliberately does not consult the usage-limit filter the sweep applies. An
// operator asking about one object is not the same as a background sweep
// spending an unattended budget, and refusing to answer because a backend is
// near its egress cap would make the command useless exactly when it is most
// needed.
//
// A mismatch is handled identically to one the sweep finds: the bytes are
// discarded and the ledger row dropped, so the replicator rebuilds from a
// healthy copy.
func (s *Scrubber) ScrubKey(ctx context.Context, key string) ([]CopyVerification, error) {
	ctx = audit.WithRequestID(ctx, audit.NewID())
	ctx, span := telemetry.StartSpan(ctx, "ScrubKey")
	defer span.End()

	locations, err := s.store.GetAllObjectLocations(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("failed to look up copies of %s: %w", key, err)
	}

	results := make([]CopyVerification, 0, len(locations))
	for i := range locations {
		results = append(results, s.verifyCopy(ctx, &locations[i]))
	}

	audit.Log(ctx, "integrity.scrub_key",
		slog.String("key", key),
		slog.Int("copies", len(results)),
	)
	return results, nil
}

// verifyCopy maps one copy's verification onto the reported outcome. A copy
// with no stored hash short-circuits: reading it would spend egress to compare
// against nothing.
func (s *Scrubber) verifyCopy(ctx context.Context, loc *core.ObjectLocation) CopyVerification {
	if loc.ContentHash == "" {
		return CopyVerification{Backend: loc.BackendName, Outcome: CopyNotHashed}
	}

	switch s.verifyOne(ctx, loc).Outcome {
	case ItemSucceeded:
		return CopyVerification{Backend: loc.BackendName, Outcome: CopyVerified}
	case ItemFailed:
		return CopyVerification{Backend: loc.BackendName, Outcome: CopyMismatch}
	default:
		return CopyVerification{Backend: loc.BackendName, Outcome: CopyUnreadable}
	}
}

// number of objects checked and the number of hash mismatches found.
func (s *Scrubber) Scrub(ctx context.Context, batchSize int, observer progress.Observer) WorkSummary {
	ctx = audit.WithRequestID(ctx, audit.NewID())
	ctx, span := telemetry.StartSpan(ctx, "Scrub")
	defer span.End()

	affordable, declined := s.affordableBackends()

	locs, err := s.store.GetLeastRecentlyScrubbedObjects(ctx, batchSize, affordable)
	if err != nil {
		s.log.ErrorContext(ctx, "failed to fetch objects", "error", err)
		return WorkSummary{}
	}

	deferred := s.countDeferred(ctx, declined)

	// Published after the cycle so the gauges reflect the work just done.
	// Deferred copies are excluded from the batch but not from the gauges: they
	// were not verified, so the coverage age must keep climbing rather than
	// reporting the fleet as checked.
	defer s.reportCoverage(ctx)

	// Scrub stays sequential (Concurrency 1): each item reads and hashes a full
	// object body, so a wider window can hammer the backends. Concurrency is a
	// parameter now, so raising it later is a config change, not a rewrite.
	runner := BatchRunner[core.ObjectLocation]{
		Name:        "scrub",
		Log:         s.log,
		Concurrency: 1,
		Observer:    observer,
		// Copies are verified per (key, backend), so a replicated object is
		// scrubbed once per backend. Naming the backend keeps those from
		// reading as the same object listed twice.
		Key: func(l core.ObjectLocation) string { return l.ObjectKey + " [" + l.BackendName + "]" },
	}
	sum := runner.Run(ctx, locs, func(ctx context.Context, loc core.ObjectLocation) ItemResult {
		res := s.verifyOne(ctx, &loc)
		// "checked" = an object we actually verified (matched or mismatched); a
		// verify error is skipped, not checked.
		if res.Outcome != ItemSkipped {
			telemetry.IntegrityChecksTotal.WithLabelValues("scrub").Inc()
		}
		return res
	})
	sum.Deferred = deferred
	return sum
}

// affordableBackends splits the fleet into the backends the scrubber can still
// read from and the ones whose usage limits it would breach.
//
// The check asks only for headroom, not for a specific object's size, because
// the split decides which backends a batch may be drawn from before any object
// is known. verifyOne re-checks against the real size.
func (s *Scrubber) affordableBackends() (affordable, declined []string) {
	order := s.deps.BackendOrder()
	affordable = s.deps.Usage().BackendsWithinLimits(order, 1, 0, 0)

	if len(affordable) == len(order) {
		return affordable, nil
	}
	keep := make(map[string]bool, len(affordable))
	for _, name := range affordable {
		keep[name] = true
	}
	for _, name := range order {
		if !keep[name] {
			declined = append(declined, name)
		}
	}
	return affordable, declined
}

// countDeferred reports how many scrubbable copies sit on backends this cycle
// declined to read. Counting the queue rather than the batch is the point: the
// batch never contained them, so it cannot say how much was left undone.
func (s *Scrubber) countDeferred(ctx context.Context, declined []string) int {
	if len(declined) == 0 {
		return 0
	}
	n, err := s.store.CountScrubCandidatesOnBackends(ctx, declined)
	if err != nil {
		s.log.WarnContext(ctx, "failed to count deferred scrub candidates", "error", err)
		return 0
	}
	telemetry.UsageLimitRejectionsTotal.WithLabelValues("scrub", "read").Add(float64(n))
	s.log.WarnContext(ctx, "scrub deferred copies on backends over their usage limit",
		"backends", declined, "copies", n)
	return int(n)
}

// reportCoverage publishes how far behind verification is, which is what says
// whether the scrubber is keeping up with the fleet rather than merely running.
func (s *Scrubber) reportCoverage(ctx context.Context) {
	age, neverVerified, err := s.store.OldestUnverifiedAge(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "failed to read scrub coverage", "error", err)
		return
	}
	telemetry.IntegrityOldestUnverifiedSeconds.Set(age.Seconds())
	telemetry.IntegrityNeverVerifiedCopies.Set(float64(neverVerified))
}

// verifyOne verifies one object's stored hash and classifies the result for the
// batch tally: a matched hash succeeds, a mismatch fails, and a verify error is
// skipped (not counted as checked). The returned Status feeds the progress
// stream the BatchRunner brackets each item with.
func (s *Scrubber) verifyOne(ctx context.Context, loc *core.ObjectLocation) ItemResult {
	// The batch-level split only asked whether the backend had any headroom
	// at all, before any object was known. A sweep reads every copy in the
	// fleet, so the object's own size is admitted too, or a batch admitted on
	// a sliver of remaining budget reads straight through it.
	//
	// Declined ahead of the scrub stamp below: a copy that was never read has
	// not been verified, and recording it as scrubbed would send it to the
	// back of the queue claiming an integrity check that never happened.
	if !s.deps.Usage().WithinLimits(loc.BackendName, 1, loc.SizeBytes, 0) {
		telemetry.IntegrityUsageDeclinedTotal.Inc()
		s.log.WarnContext(ctx, "scrub declined by usage limits",
			"key", loc.ObjectKey, "backend", loc.BackendName, "size", loc.SizeBytes)
		return ItemResult{Outcome: ItemSkipped, Status: progress.StatusSkipped}
	}

	match, verifyErr := s.verifyObject(ctx, loc)

	// Stamped even when the read failed: a copy that always fails would
	// otherwise sit at the head of the queue and starve the rest of the sweep.
	if err := s.store.MarkObjectScrubbed(ctx, loc.ObjectKey, loc.BackendName); err != nil {
		s.log.WarnContext(ctx, "failed to record scrub timestamp",
			"key", loc.ObjectKey, "backend", loc.BackendName, "error", err)
	}

	if verifyErr != nil {
		s.log.WarnContext(ctx, "failed to verify object",
			"key", loc.ObjectKey, "backend", loc.BackendName, "error", verifyErr)
		return ItemResult{Outcome: ItemSkipped, Status: progress.StatusUnreadable}
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
		s.dropCorruptedLocation(ctx, loc)
		return false, nil
	}

	return true, nil
}

// dropCorruptedLocation removes the ledger row for a discarded copy. Without
// it the replicator still counts the copy and never rebuilds the object.
func (s *Scrubber) dropCorruptedLocation(ctx context.Context, loc *core.ObjectLocation) {
	if err := s.store.DeleteObjectLocation(ctx, loc.ObjectKey, loc.BackendName); err != nil {
		s.log.ErrorContext(ctx, "failed to drop location for corrupted copy",
			"key", loc.ObjectKey, "backend", loc.BackendName, "error", err)
		return
	}
	audit.Log(ctx, "integrity.copy_discarded",
		slog.String("key", loc.ObjectKey),
		slog.String("backend", loc.BackendName),
	)
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

// readAndHash returns the SHA-256 hex digest of the bytes the client wrote for
// this copy. The digest has to match what the replicator computes for the same
// copy, so the work lives in storedHasher rather than here.
func (s *Scrubber) readAndHash(ctx context.Context, loc *core.ObjectLocation) (string, error) {
	return s.hasher.hashStored(ctx, loc)
}
