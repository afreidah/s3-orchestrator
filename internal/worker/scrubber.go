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
	"github.com/afreidah/s3-orchestrator/internal/observe/cycle"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/proxy/etag"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/storedform"
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
	DeleteObjectLocation(ctx context.Context, loc *core.ObjectLocation, keepAtLeast int, reason string) ([]core.CleanupItem, error)
	GetAllObjectLocations(ctx context.Context, key string) ([]core.ObjectLocation, error)
	RecordObjectIdentity(ctx context.Context, loc *core.ObjectLocation, id *core.ObjectIdentity) error
	UpdateCopyMetadata(ctx context.Context, loc *core.ObjectLocation, p *core.CopyPatch) error
}

// Scrubber periodically verifies stored object integrity by reading objects
// from backends, computing their SHA-256 hash, and comparing against the
// stored content hash. Also supports backfilling hashes for objects that
// were written before integrity was enabled. The embedded Section reads its
// integrity config from the running configuration.
type Scrubber struct {
	syncutil.Section[config.IntegrityConfig]
	log       *slog.Logger
	deps      Ops
	placement Placement
	store     ScrubberStore
	hasher    *storedHasher
	telemetry.SharedGauges
}

// ScrubberDeps groups the scrubber's constructor dependencies. Encryptor and
// Codec are optional, and are what the stored form has to be undone through
// before hashing; a copy recorded as encrypted or compressed cannot be verified
// without the matching one.
type ScrubberDeps struct {
	Ops       Ops
	Placement Placement
	Store     ScrubberStore
	Config    syncutil.Section[config.IntegrityConfig]
	Encryptor *encryption.Encryptor
	Codec     storedform.Decompressor
}

// NewScrubber creates a Scrubber with the given dependencies.
func NewScrubber(deps *ScrubberDeps) *Scrubber {
	must.NotNil("Ops", deps.Ops)
	must.NotNil("Placement", deps.Placement)
	must.NotNil("Store", deps.Store)
	must.NotNil("Config", deps.Config)
	return &Scrubber{
		Section:   deps.Config,
		deps:      deps.Ops,
		placement: deps.Placement,
		store:     deps.Store,
		hasher:    newStoredHasher(deps.Ops, deps.Encryptor, deps.Codec),
		log:       workerLogger(audit.WorkloadScrub),
	}
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

// ScrubKey verifies every copy of one key immediately and reports each
// separately. Unlike the sweep, it holds no scrub reserve back: a read is
// admitted as a client read is, against the backend's whole egress limit, and
// a copy the limit refuses is reported declined rather than unreadable.
//
// A mismatch is handled as the sweep handles one: the bytes are discarded and
// the ledger row dropped, so the replicator rebuilds from a healthy copy.
func (s *Scrubber) ScrubKey(ctx context.Context, key string) ([]CopyVerification, error) {
	ctx = audit.WithWorkload(ctx, audit.WorkloadScrub)
	ctx, span := telemetry.StartSpan(ctx, "scrub_key", telemetry.AttrOperation.String("scrub_key"))
	defer span.End()

	locations, err := s.store.GetAllObjectLocations(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("failed to look up copies of %s: %w", key, err)
	}
	results := make([]CopyVerification, 0, len(locations))
	for i := range locations {
		outcome := s.checkCopy(ctx, &locations[i], s.hasher.admitsRead)
		results = append(results, CopyVerification{Backend: locations[i].BackendName, Outcome: outcome})
	}
	audit.Log(ctx, "integrity.scrub_key",
		slog.String("key", key),
		slog.Int("copies", len(results)),
	)
	return results, nil
}

// Scrub verifies a batch of copies with stored content hashes, as one scrub
// pass. backend, when set, limits the pass to that backend's copies.
func (s *Scrubber) Scrub(ctx context.Context, batchSize int, backend string, observer progress.Observer) (batch.Summary, error) {
	return cycle.Run(ctx, cycle.Pass{Workload: audit.WorkloadScrub}, func(ctx context.Context) (batch.Summary, error) {
		return s.scrub(ctx, batchSize, backend, observer)
	})
}

// scrub is the body of Scrub after the span is open.
func (s *Scrubber) scrub(ctx context.Context, batchSize int, backend string, observer progress.Observer) (batch.Summary, error) {
	affordable, declined := s.affordableBackends()
	affordable = restrictToBackend(affordable, backend)

	// One cutoff for both queries, so the deferred count and the batch describe
	// the same population even if the cycle straddles a second boundary.
	scrubbedBefore := s.scrubbedBefore()

	locs, err := s.store.GetLeastRecentlyScrubbedObjects(ctx, batchSize, affordable, scrubbedBefore)
	if err != nil {
		return batch.Summary{}, fmt.Errorf("fetch copies to scrub: %w", err)
	}

	deferred := s.countDeferred(ctx, declined, scrubbedBefore)

	// Published after the cycle so the gauges reflect the work just done.
	// Deferred copies get their own gauge rather than being folded into the
	// coverage age: the sweep cannot stamp them, so counting them there would
	// pin the age to wall clock and no amount of scrubbing would lower it.
	defer s.reportCoverage(ctx, affordable)

	// Scrub stays sequential (Concurrency 1): each item reads and hashes a full
	// object body, so a wider window can hammer the backends.
	runner := batch.Runner[core.ObjectLocation]{
		Concurrency: 1,
		Observer:    observer,
		// Copies are verified per (key, backend), so a replicated object is
		// scrubbed once per backend. Naming the backend keeps those from
		// reading as the same object listed twice.
		Key: func(l core.ObjectLocation) string { return l.ObjectKey + " [" + l.BackendName + "]" },
	}
	sum := runner.Run(ctx, locs, func(ctx context.Context, loc core.ObjectLocation) batch.ItemResult {
		return scrubResult(s.checkCopy(ctx, &loc, s.admits))
	})
	sum.Deferred = deferred
	return sum, nil
}

// scrubResult folds a copy's verification into the sweep's tally: a verified
// copy succeeds, a mismatch fails, and a copy that could not be checked is
// skipped with the reason on the progress stream.
func scrubResult(outcome CopyOutcome) batch.ItemResult {
	switch outcome {
	case CopyVerified:
		return batch.Succeeded()
	case CopyMismatch:
		return batch.ItemResult{Outcome: batch.ItemFailed, Status: "mismatch"}
	case CopyDeclined:
		return batch.Skipped(progress.StatusSkipped)
	default:
		return batch.Skipped(progress.StatusUnreadable)
	}
}

// scrubbedBefore returns the re-verification cutoff for this cycle. A missing
// config falls back to the default floor rather than to no floor: an unset
// value must not turn every pass into a full re-read of each backend.
func (s *Scrubber) scrubbedBefore() time.Time {
	return s.Config().ScrubbedBefore(time.Now())
}

// ReachableBackends returns the backends the scrub queue draws from right now,
// so the dashboard scopes coverage to exactly the copies the sweep can stamp.
func (s *Scrubber) ReachableBackends() []string {
	affordable, _ := s.affordableBackends()
	return affordable
}

// affordableBackends splits the fleet into the backends the scrubber can still
// read from and the ones it has to leave alone. It checks only for headroom,
// since no object is known yet; verifyOne re-checks against the real size.
func (s *Scrubber) affordableBackends() (affordable, declined []string) {
	for _, name := range s.deps.BackendOrder() {
		if s.admits(name, 0) {
			affordable = append(affordable, name)
		} else {
			declined = append(declined, name)
		}
	}
	return affordable, declined
}

// admits reports whether the sweep may read egress bytes from a backend. The
// read is checked as if it also spent the scrub reserve, so the sweep stops
// while that share of the monthly egress limit is still free for client reads,
// which are checked against the whole limit.
func (s *Scrubber) admits(backend string, egress int64) bool {
	reserve := s.Config().ScrubReserve(s.deps.Usage().GetLimits()[backend].EgressByteLimit)
	return s.deps.Acct().Allow(backend, s3op.GetObject, egress+reserve, 0)
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
	s.log.WarnContext(ctx, "scrub deferred copies on backends past their usage limit or scrub reserve",
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
	s.PublishGauges(ctx, telemetry.GaugeSourceScrubber, telemetry.WorkerGauges{
		OldestUnverifiedSeconds: new(stat.OldestUnverifiedAge.Seconds()),
		NeverVerifiedCopies:     &stat.NeverVerified,
		DeferredCopies:          &stat.Deferred,
	})
}

// checkCopy verifies one copy against its own stored hash, admitting the read
// through admit, and acts on what it found. A copy that was read is stamped
// as scrubbed whatever the outcome, and a mismatch is discarded so the
// replicator rebuilds from a healthy copy.
func (s *Scrubber) checkCopy(ctx context.Context, loc *core.ObjectLocation, admit func(backend string, egress int64) bool) CopyOutcome {
	outcome := s.hasher.verify(ctx, loc, loc, integrityOpScrub, admit)
	if outcome == CopyNotHashed || outcome == CopyDeclined {
		return outcome
	}

	// Stamped even when the read failed: a copy that always fails would
	// otherwise sit at the head of the queue and starve the rest of the sweep.
	// A copy a newer write replaced is not stamped, since what was read is gone.
	err := s.store.UpdateCopyMetadata(ctx, loc, &core.CopyPatch{Scrubbed: true})
	if err != nil && !errors.Is(err, core.ErrCopyChanged) {
		s.log.WarnContext(ctx, "failed to record scrub timestamp",
			"key", loc.ObjectKey, "backend", loc.BackendName, "error", err)
	}

	if outcome == CopyMismatch {
		if err := s.discardCopy(ctx, loc, "integrity_scrub_failed"); err != nil {
			s.log.ErrorContext(ctx, "failed to drop location for corrupted copy",
				"key", loc.ObjectKey, "backend", loc.BackendName, "error", err)
		}
	}
	return outcome
}

// discardCopy removes a copy's ledger row and quota and queues its bytes in
// one transaction, then settles the cleanup. Without the row removal the
// replicator still counts the copy and never rebuilds the object. A copy a
// client overwrite or delete replaced since it was read is left alone: that
// write already queued the discarded bytes.
func (s *Scrubber) discardCopy(ctx context.Context, loc *core.ObjectLocation, reason string) error {
	err := s.placement.DropCopy(ctx, loc, 0, reason)
	switch {
	case errors.Is(err, core.ErrCopyChanged):
		return nil
	case err != nil:
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

// unreadableCopies selects copies encrypted with no key, which nothing can
// decrypt.
var unreadableCopies = core.FormFilter{With: core.StoredEncrypted, Without: core.StoredKeyed}

// ListUnreadable returns up to limit copies that are encrypted with no key,
// and how many there are in total.
func (s *Scrubber) ListUnreadable(ctx context.Context, limit int) ([]core.ObjectLocation, int64, error) {
	locs, err := s.store.ListCopiesByForm(ctx, &unreadableCopies, limit, core.Cursor{})
	if err != nil {
		return nil, 0, err
	}
	count, err := s.store.CountCopiesByForm(ctx, unreadableCopies.With, unreadableCopies.Without)
	if err != nil {
		return nil, 0, err
	}
	return locs, count, nil
}

// PurgeUnreadable discards every copy that is encrypted with no key, batchSize
// at a time, as one purge pass. Nothing can decrypt them, so they only hold
// quota. The listing is walked by cursor, so a copy that fails to discard is
// tried once per purge.
func (s *Scrubber) PurgeUnreadable(ctx context.Context, batchSize int, observer progress.Observer) (batch.Summary, error) {
	return cycle.Run(ctx, cycle.Pass{Workload: audit.WorkloadPurge}, func(ctx context.Context) (batch.Summary, error) {
		pager := copyPager(batchSize, func(ctx context.Context, limit int, after core.Cursor) ([]core.ObjectLocation, error) {
			return s.store.ListCopiesByForm(ctx, &unreadableCopies, limit, after)
		})
		runner := batch.Runner[core.ObjectLocation]{Concurrency: 1, Observer: observer, Key: locationKey}
		total, stop, err := pager.RunPages(ctx, runner, func(ctx context.Context, loc core.ObjectLocation) batch.ItemResult {
			if err := s.discardCopy(ctx, &loc, "unreadable_purged"); err != nil {
				s.log.WarnContext(ctx, "failed to purge unreadable copy",
					"key", loc.ObjectKey, "backend", loc.BackendName, "error", err)
				return batch.Failed()
			}
			return batch.Succeeded()
		})
		if stop == batch.Errored {
			return total, fmt.Errorf("list unreadable copies: %w", err)
		}
		return total, nil
	})
}

// copyPager walks a listing of copies by key, batchSize at a time.
func copyPager(batchSize int, list func(ctx context.Context, limit int, after core.Cursor) ([]core.ObjectLocation, error)) batch.Pager[core.ObjectLocation, core.Cursor] {
	return batch.Pager[core.ObjectLocation, core.Cursor]{
		PageSize: batch.FixedPage(batchSize),
		List:     list,
		CursorOf: core.CursorOf,
	}
}

// locationKey labels a copy on the progress stream by its object key. A
// runner hands each item by value, which is why this takes the copy whole.
var locationKey = func(loc core.ObjectLocation) string { return loc.ObjectKey }

// -------------------------------------------------------------------------
// BACKFILL  -  compute hashes for objects that don't have one
// -------------------------------------------------------------------------

// BackfillRequest bounds one backfill run. BatchSize copies are hashed per
// page, with Pause between pages to rate-limit backend reads. MaxObjects
// caps the copies hashed, and zero drains the whole backlog. Backend, when
// set, limits the run to that backend's copies.
type BackfillRequest struct {
	BatchSize  int
	MaxObjects int
	Pause      time.Duration
	Backend    string
}

// BackfillResult reports one backfill run: Succeeded counts the copies hashed
// and Skipped the ones that could not be decoded. Done is true only when the
// backlog drained; a run stopped by the object cap or a cancelled context
// reports false so the caller knows more work remains.
type BackfillResult struct {
	batch.Summary
	Done bool
}

// Backfill computes and stores the SHA-256 digest of every copy that has
// none, a page at a time, as one backfill pass. It stops when the backlog
// drains, the object cap is hit, or ctx is cancelled. observer, when non-nil,
// receives a start and end step per copy hashed.
func (s *Scrubber) Backfill(ctx context.Context, req BackfillRequest, observer progress.Observer) (BackfillResult, error) {
	return cycle.Run(ctx, cycle.Pass{Workload: audit.WorkloadBackfill}, func(ctx context.Context) (BackfillResult, error) {
		return s.backfill(ctx, req, observer)
	})
}

// backfill is the body of Backfill after the span is open. The cap and the
// pause sit between pages, so stopping on them leaves the page that was just
// hashed complete.
func (s *Scrubber) backfill(ctx context.Context, req BackfillRequest, observer progress.Observer) (BackfillResult, error) {
	unhashed := core.FormFilter{Without: core.StoredHashed, Backend: req.Backend, ManagedOnly: true}
	pager := copyPager(req.BatchSize, func(ctx context.Context, limit int, after core.Cursor) ([]core.ObjectLocation, error) {
		return s.store.ListCopiesByForm(ctx, &unhashed, limit, after)
	})
	var res BackfillResult
	stop, err := pager.Walk(ctx, func(ctx context.Context, locs []core.ObjectLocation) (batch.Step, error) {
		res.Summary = res.Plus(s.hashPage(ctx, locs, observer))
		if req.MaxObjects > 0 && res.Succeeded >= req.MaxObjects {
			return batch.Step{Stop: true}, nil
		}
		if len(locs) < req.BatchSize {
			return batch.Step{}, nil // the last page, so there is nothing to pause before
		}
		return batch.Step{Stop: req.Pause > 0 && !sleepOrCancel(ctx, req.Pause)}, nil
	})
	res.Done = stop == batch.Exhausted
	if stop == batch.Errored {
		return res, fmt.Errorf("list unhashed copies: %w", err)
	}
	return res, nil
}

// hashPage hashes one page of copies, one at a time like Scrub, since each
// reads and hashes a full object body.
func (s *Scrubber) hashPage(ctx context.Context, locs []core.ObjectLocation, observer progress.Observer) batch.Summary {
	runner := batch.Runner[core.ObjectLocation]{Concurrency: 1, Observer: observer, Key: locationKey}
	return runner.Run(ctx, locs, func(ctx context.Context, loc core.ObjectLocation) batch.ItemResult {
		return s.hashOne(ctx, &loc)
	})
}

// sleepOrCancel waits for d or for ctx to be cancelled, returning false when
// cancellation wins so the caller stops early.
func sleepOrCancel(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// hashOne computes and stores the hash for one object, returning the outcome
// for the batch tally and a status for the progress stream.
func (s *Scrubber) hashOne(ctx context.Context, loc *core.ObjectLocation) batch.ItemResult {
	digests, hashErr := s.hasher.hashStored(ctx, loc)
	if undecodable(hashErr) {
		// No later pass could hash it either, so it is not a failure.
		s.log.WarnContext(ctx, "skipping object that cannot be decoded",
			"key", loc.ObjectKey, "backend", loc.BackendName, "error", hashErr)
		return batch.Skipped(progress.StatusUnreadable)
	}
	if hashErr != nil {
		s.log.WarnContext(ctx, "failed to hash object",
			"key", loc.ObjectKey, "backend", loc.BackendName, "error", hashErr)
		return batch.Failed()
	}
	// The pass read the whole body to produce the digest, so the copy is
	// verified by construction and stamped as scrubbed in the same write.
	err := s.store.UpdateCopyMetadata(ctx, loc, &core.CopyPatch{ContentHash: digests.SHA256, Scrubbed: true})
	if errors.Is(err, core.ErrCopyChanged) {
		// A newer write replaced the copy mid-hash; its own write recorded a hash.
		return batch.Skipped(progress.StatusSkipped)
	}
	if err != nil {
		s.log.WarnContext(ctx, "failed to store hash",
			"key", loc.ObjectKey, "backend", loc.BackendName, "error", err)
		return batch.Failed()
	}
	s.recordETag(ctx, loc, digests.MD5)
	return batch.Succeeded()
}

// undecodable reports whether a hashing error is permanent rather than an outage
// that clears on its own.
func undecodable(err error) bool {
	return errors.Is(err, core.ErrEncryptionFlagMismatch) ||
		errors.Is(err, storedform.ErrNoCodec) || errors.Is(err, storedform.ErrNoEncryptor)
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
	if err := s.store.RecordObjectIdentity(ctx, loc, id); err != nil {
		s.log.WarnContext(ctx, "failed to record object etag",
			"key", loc.ObjectKey, "backend", loc.BackendName, "error", err)
	}
}
