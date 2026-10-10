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
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/proxy/etag"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// ScrubberStore is the narrow persistence surface the scrubber needs:
// integrity row reads/writes, removal of a location whose bytes failed
// verification, and the copies of one key for an on-demand verification.
// It also records a plaintext ETag for an object that has none.
type ScrubberStore interface {
	core.IntegrityStore
	LedgerStats(ctx context.Context) (core.LedgerStats, error)
	DeleteObjectLocation(ctx context.Context, key, backendName string) (int64, error)
	GetAllObjectLocations(ctx context.Context, key string) ([]core.ObjectLocation, error)
	RecordObjectIdentity(ctx context.Context, key string, id *core.ObjectIdentity) error
}

// Scrubber periodically verifies stored object integrity by reading objects
// from backends, computing their SHA-256 hash, and comparing against the
// stored content hash. Also supports backfilling hashes for objects that
// were written before integrity was enabled.
type Scrubber struct {
	log       *slog.Logger
	deps      Ops
	placement Placement
	store     ScrubberStore
	hasher    *storedHasher
	gauges    GaugePublisher
	cfg       syncutil.AtomicConfig[config.IntegrityConfig]
}

// ScrubberDeps groups the scrubber's constructor dependencies. Encryptor and
// Codec are optional, and are what the stored form has to be undone through
// before hashing; a copy recorded as encrypted or compressed cannot be verified
// without the matching one.
type ScrubberDeps struct {
	Ops       Ops
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

// SetGaugePublisher shares verification coverage with every instance. Called
// once at wiring, before the first pass; without it the gauges are set locally.
func (s *Scrubber) SetGaugePublisher(p GaugePublisher) {
	s.gauges = p
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

// CopyVerification is the outcome of verifying one copy of one key. A copy
// with no stored hash reports NotHashed, not verified.
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
// separately. Unlike the sweep, it ignores usage limits, since an operator
// checking one object should get an answer even near an egress cap.
//
// A mismatch is handled as the sweep handles one: the bytes are discarded and
// the ledger row dropped, so the replicator rebuilds from a healthy copy.
func (s *Scrubber) ScrubKey(ctx context.Context, key string) ([]CopyVerification, error) {
	return runOpsCycle(ctx, "ScrubKey", "scrub_key", func(ctx context.Context) ([]CopyVerification, error) {
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
	})
}

// verifyCopy maps one copy's verification onto the reported outcome. A copy
// with no stored hash short-circuits: reading it would spend egress to compare
// against nothing.
func (s *Scrubber) verifyCopy(ctx context.Context, loc *core.ObjectLocation) CopyVerification {
	if loc.ContentHash == "" {
		return CopyVerification{Backend: loc.BackendName, Outcome: CopyNotHashed}
	}

	switch s.verifyOne(ctx, loc).Outcome {
	case batch.ItemSucceeded:
		return CopyVerification{Backend: loc.BackendName, Outcome: CopyVerified}
	case batch.ItemFailed:
		return CopyVerification{Backend: loc.BackendName, Outcome: CopyMismatch}
	default:
		return CopyVerification{Backend: loc.BackendName, Outcome: CopyUnreadable}
	}
}

// Scrub verifies a batch of objects with stored content hashes. Returns the
// number of objects checked and the number of hash mismatches found.
func (s *Scrubber) Scrub(ctx context.Context, batchSize int, backend string, observer progress.Observer) batch.Summary {
	ctx = audit.WithRequestID(ctx, audit.NewID())
	return runTickCycle(ctx, "Scrub", "scrub", func(ctx context.Context) batch.Summary {
		return s.scrub(ctx, batchSize, backend, observer)
	})
}

// scrub is the body of Scrub after the span is open.
func (s *Scrubber) scrub(ctx context.Context, batchSize int, backend string, observer progress.Observer) batch.Summary {
	affordable, declined := s.affordableBackends()
	affordable = restrictToBackend(affordable, backend)

	// One cutoff for both queries, so the deferred count and the batch describe
	// the same population even if the cycle straddles a second boundary.
	scrubbedBefore := s.scrubbedBefore()

	locs, err := s.store.GetLeastRecentlyScrubbedObjects(ctx, batchSize, affordable, scrubbedBefore)
	if err != nil {
		s.log.ErrorContext(ctx, "failed to fetch objects", "error", err)
		return batch.Summary{}
	}

	deferred := s.countDeferred(ctx, declined, scrubbedBefore)

	// Published after the cycle so the gauges reflect the work just done.
	// Deferred copies get their own gauge rather than being folded into the
	// coverage age: the sweep cannot stamp them, so counting them there would
	// pin the age to wall clock and no amount of scrubbing would lower it.
	defer s.reportCoverage(ctx, affordable)

	// Scrub stays sequential (Concurrency 1): each item reads and hashes a full
	// object body, so a wider window can hammer the backends. Concurrency is a
	// parameter now, so raising it later is a config change, not a rewrite.
	runner := batch.Runner[core.ObjectLocation]{
		Name:        "scrub",
		Log:         s.log,
		Concurrency: 1,
		Observer:    observer,
		// Copies are verified per (key, backend), so a replicated object is
		// scrubbed once per backend. Naming the backend keeps those from
		// reading as the same object listed twice.
		Key: func(l core.ObjectLocation) string { return l.ObjectKey + " [" + l.BackendName + "]" },
	}
	sum := runner.Run(ctx, locs, func(ctx context.Context, loc core.ObjectLocation) batch.ItemResult {
		res := s.verifyOne(ctx, &loc)
		// "checked" = an object we actually verified (matched or mismatched); a
		// verify error is skipped, not checked.
		if res.Outcome != batch.ItemSkipped {
			telemetry.IntegrityChecksTotal.WithLabelValues("scrub").Inc()
		}
		return res
	})
	sum.Deferred = deferred
	return sum
}

// scrubbedBefore returns the re-verification cutoff for this cycle. A missing
// config falls back to the default floor rather than to no floor: an unset
// value must not turn every pass into a full re-read of each backend.
func (s *Scrubber) scrubbedBefore() time.Time {
	icfg := s.Config()
	if icfg == nil {
		return time.Now().Add(-config.DefaultScrubberMinAge)
	}
	return icfg.ScrubbedBefore(time.Now())
}

// affordableBackends splits the fleet into the backends the scrubber can still
// read from and the ones whose usage limits it would breach. It checks only
// for headroom, since no object is known yet; verifyOne re-checks against the
// real size.
func (s *Scrubber) affordableBackends() (affordable, declined []string) {
	order := s.deps.BackendOrder()
	affordable = s.deps.Usage().BackendsWithinLimits(order, getObjectOp, 0, 0)

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

// restrictToBackend narrows an affordable set to the one backend a caller named,
// or returns it whole when none was. A named backend the budget declined stays
// out, and its copies are reported as deferred.
func restrictToBackend(affordable []string, backend string) []string {
	if backend == "" {
		return affordable
	}
	if slices.Contains(affordable, backend) {
		return []string{backend}
	}
	return nil
}

// countDeferred reports how many scrubbable copies sit on backends this cycle
// declined to read. Counting the queue rather than the batch is the point: the
// batch never contained them, so it cannot say how much was left undone.
func (s *Scrubber) countDeferred(ctx context.Context, declined []string, scrubbedBefore time.Time) int {
	if len(declined) == 0 {
		return 0
	}
	n, err := s.store.CountScrubCandidatesOnBackends(ctx, declined, scrubbedBefore)
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
// reachable scopes the age and the never-verified count to the copies this
// cycle could have read; the rest are published as deferred.
func (s *Scrubber) reportCoverage(ctx context.Context, reachable []string) {
	ledger, err := s.store.LedgerStats(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "failed to read scrub coverage", "error", err)
		return
	}
	stat := ledger.Coverage(reachable, time.Now())
	publishGauges(ctx, s.gauges, telemetry.GaugeSourceScrubber, telemetry.WorkerGauges{
		OldestUnverifiedSeconds: new(stat.OldestUnverifiedAge.Seconds()),
		NeverVerifiedCopies:     &stat.NeverVerified,
		DeferredCopies:          &stat.Deferred,
	})
}

// verifyOne verifies one object's stored hash and classifies the result for the
// batch tally: a matched hash succeeds, a mismatch fails, and a verify error is
// skipped (not counted as checked). The returned Status feeds the progress
// stream the batch.Runner brackets each item with.
func (s *Scrubber) verifyOne(ctx context.Context, loc *core.ObjectLocation) batch.ItemResult {
	// Admit the object's own size, since the batch-level split only checked
	// for any headroom. This must run before the scrub stamp below, so an
	// unread copy is not recorded as verified.
	if !s.deps.Usage().WithinLimits(loc.BackendName, getObjectOp, loc.SizeBytes, 0) {
		telemetry.IntegrityUsageDeclinedTotal.Inc()
		s.log.WarnContext(ctx, "scrub declined by usage limits",
			"key", loc.ObjectKey, "backend", loc.BackendName, "size", loc.SizeBytes)
		return batch.ItemResult{Outcome: batch.ItemSkipped, Status: progress.StatusSkipped}
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
		return batch.ItemResult{Outcome: batch.ItemSkipped, Status: progress.StatusUnreadable}
	}
	if match {
		return batch.ItemResult{Outcome: batch.ItemSucceeded, Status: progress.StatusOK}
	}
	return batch.ItemResult{Outcome: batch.ItemFailed, Status: "mismatch"}
}

// verifyObject reads a single object, computes its hash, and compares to
// the stored content hash. On mismatch the corrupted copy is enqueued for
// cleanup. Returns true if the hash matches.
func (s *Scrubber) verifyObject(ctx context.Context, loc *core.ObjectLocation) (bool, error) {
	actual, err := s.readAndHash(ctx, loc)
	if err != nil {
		return false, err
	}

	if actual.SHA256 != loc.ContentHash {
		s.log.ErrorContext(ctx, "integrity check failed",
			"key", loc.ObjectKey, "backend", loc.BackendName,
			"expected_hash", loc.ContentHash, "actual_hash", actual.SHA256)
		telemetry.IntegrityErrorsTotal.WithLabelValues("scrub").Inc()
		event.Publish(event.IntegrityCorruptionFound, loc.ObjectKey, map[string]any{
			"key":           loc.ObjectKey,
			"backend":       loc.BackendName,
			"expected_hash": loc.ContentHash,
			"actual_hash":   actual.SHA256,
			"size_bytes":    loc.SizeBytes,
		})
		if err := s.discardCopy(ctx, loc, "integrity_scrub_failed"); err != nil {
			s.log.ErrorContext(ctx, "failed to drop location for corrupted copy",
				"key", loc.ObjectKey, "backend", loc.BackendName, "error", err)
		}
		return false, nil
	}

	return true, nil
}

// discardCopy deletes a copy's bytes, or queues them for cleanup, and removes
// its ledger row and quota. Without the row removal the replicator still counts
// the copy and never rebuilds the object.
func (s *Scrubber) discardCopy(ctx context.Context, loc *core.ObjectLocation, reason string) error {
	if be, err := s.deps.GetBackend(loc.BackendName); err == nil {
		s.placement.DeleteOrEnqueue(ctx, be, &core.CleanupRequest{
			BackendName: loc.BackendName,
			ObjectKey:   loc.ObjectKey,
			StorageKey:  core.StoragePath(loc.ObjectKey, loc.StorageKey),
			Reason:      reason,
			SizeBytes:   loc.SizeBytes,
		})
	}
	if _, err := s.store.DeleteObjectLocation(ctx, loc.ObjectKey, loc.BackendName); err != nil {
		return err
	}
	audit.Log(ctx, "integrity.copy_discarded",
		slog.String("key", loc.ObjectKey),
		slog.String("backend", loc.BackendName),
		slog.String("reason", reason),
	)
	return nil
}

// -------------------------------------------------------------------------
// UNREADABLE  -  copies encrypted with no key
// -------------------------------------------------------------------------

// ListUnreadable returns up to limit copies that are encrypted with no key,
// and how many there are in total.
func (s *Scrubber) ListUnreadable(ctx context.Context, limit int) ([]core.ObjectLocation, int64, error) {
	locs, err := s.store.ListUnreadableLocations(ctx, limit, core.Cursor{})
	if err != nil {
		return nil, 0, err
	}
	count, err := s.store.CountUnreadableLocations(ctx)
	if err != nil {
		return nil, 0, err
	}
	return locs, count, nil
}

// PurgeUnreadable discards every copy that is encrypted with no key, batchSize
// at a time. Nothing can decrypt them, so they only hold quota. The listing is
// walked by cursor, so a copy that fails to discard is tried once per purge.
func (s *Scrubber) PurgeUnreadable(ctx context.Context, batchSize int, observer progress.Observer) batch.Summary {
	pager := batch.Pager[core.ObjectLocation, core.Cursor]{
		PageSize: batch.FixedPage(batchSize),
		List: func(ctx context.Context, limit int, after core.Cursor) ([]core.ObjectLocation, error) {
			return s.store.ListUnreadableLocations(ctx, limit, after)
		},
		CursorOf: func(loc core.ObjectLocation) core.Cursor {
			return core.Cursor{ObjectKey: loc.ObjectKey, BackendName: loc.BackendName}
		},
	}
	runner := batch.Runner[core.ObjectLocation]{
		Name:        "purge-unreadable",
		Log:         s.log,
		Concurrency: 1,
		Observer:    observer,
		Key:         func(l core.ObjectLocation) string { return l.ObjectKey },
	}
	var total batch.Summary
	stop, err := pager.Walk(ctx, func(ctx context.Context, locs []core.ObjectLocation) (batch.Step, error) {
		sum := runner.Run(ctx, locs, func(ctx context.Context, loc core.ObjectLocation) batch.ItemResult {
			if err := s.discardCopy(ctx, &loc, "unreadable_purged"); err != nil {
				s.log.WarnContext(ctx, "failed to purge unreadable copy",
					"key", loc.ObjectKey, "backend", loc.BackendName, "error", err)
				return batch.ItemResult{Outcome: batch.ItemFailed, Status: progress.StatusFailed}
			}
			return batch.ItemResult{Outcome: batch.ItemSucceeded, Status: progress.StatusOK}
		})
		total = total.Plus(sum)
		return batch.Step{Progress: sum.Succeeded}, nil
	})
	if stop == batch.Errored {
		s.log.ErrorContext(ctx, "failed to list unreadable copies", "error", err)
	}
	return total
}

// -------------------------------------------------------------------------
// BACKFILL  -  compute hashes for objects that don't have one
// -------------------------------------------------------------------------

// HashCopies computes and stores the SHA-256 digest of each copy, for a
// backfill of copies that have none. observer, when non-nil, receives a start
// step before each copy is hashed and an end step after, carrying its outcome.
func (s *Scrubber) HashCopies(ctx context.Context, locs []core.ObjectLocation, observer progress.Observer) batch.Summary {
	ctx = audit.WithRequestID(ctx, audit.NewID())
	ctx, span := telemetry.StartSpan(ctx, "Backfill")
	defer span.End()

	s.log.InfoContext(ctx, "backfill batch starting", "objects", len(locs))

	// Sequential (Concurrency 1) like Scrub: each item reads and hashes a full
	// object body.
	runner := batch.Runner[core.ObjectLocation]{
		Name:        "backfill",
		Log:         s.log,
		Concurrency: 1,
		Observer:    observer,
		Key:         func(l core.ObjectLocation) string { return l.ObjectKey },
	}
	return runner.Run(ctx, locs, func(ctx context.Context, loc core.ObjectLocation) batch.ItemResult {
		return s.hashOne(ctx, &loc)
	})
}

// hashOne computes and stores the hash for one object, returning the outcome
// for the batch tally and a status for the progress stream.
func (s *Scrubber) hashOne(ctx context.Context, loc *core.ObjectLocation) batch.ItemResult {
	digests, hashErr := s.readAndHash(ctx, loc)
	if undecodable(hashErr) {
		// No later pass could hash it either, so it is not a failure.
		s.log.WarnContext(ctx, "skipping object that cannot be decoded",
			"key", loc.ObjectKey, "backend", loc.BackendName, "error", hashErr)
		return batch.ItemResult{Outcome: batch.ItemSkipped, Status: progress.StatusUnreadable}
	}
	if hashErr != nil {
		s.log.WarnContext(ctx, "failed to hash object",
			"key", loc.ObjectKey, "backend", loc.BackendName, "error", hashErr)
		return batch.ItemResult{Outcome: batch.ItemFailed, Status: progress.StatusFailed}
	}
	if err := s.store.UpdateContentHash(ctx, loc.ObjectKey, loc.BackendName, digests.SHA256); err != nil {
		s.log.WarnContext(ctx, "failed to store hash",
			"key", loc.ObjectKey, "backend", loc.BackendName, "error", err)
		return batch.ItemResult{Outcome: batch.ItemFailed, Status: progress.StatusFailed}
	}
	s.recordETag(ctx, loc, digests.MD5)
	return batch.ItemResult{Outcome: batch.ItemSucceeded, Status: progress.StatusOK}
}

// undecodable reports whether a hashing error is permanent rather than an outage
// that clears on its own.
func undecodable(err error) bool {
	return errors.Is(err, core.ErrEncryptionFlagMismatch) || errors.Is(err, errNoCodec)
}

// recordETag gives an object with no recorded ETag the one this pass just
// computed from the plaintext. For a compressed or encrypted object this is
// the only source, since the backend's ETag describes the stored bytes. The
// store fills only a NULL ETag, so an existing one is kept.
func (s *Scrubber) recordETag(ctx context.Context, loc *core.ObjectLocation, digest string) {
	if digest == "" {
		return
	}
	id := &core.ObjectIdentity{ETag: etag.Single(digest)}
	if err := s.store.RecordObjectIdentity(ctx, loc.ObjectKey, id); err != nil {
		s.log.WarnContext(ctx, "failed to record object etag",
			"key", loc.ObjectKey, "backend", loc.BackendName, "error", err)
	}
}

// -------------------------------------------------------------------------
// SHARED  -  read object from backend, decrypt if needed, compute SHA-256
// -------------------------------------------------------------------------

// readAndHash returns the digests of the bytes the client wrote for this copy.
// They have to match what the replicator computes for the same copy, so the
// work lives in storedHasher rather than here.
func (s *Scrubber) readAndHash(ctx context.Context, loc *core.ObjectLocation) (storedDigests, error) {
	return s.hasher.hashStored(ctx, loc)
}
