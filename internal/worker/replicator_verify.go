// -------------------------------------------------------------------------------
// Replica Verification - Hash-Check a New Copy Before Recording It
//
// Author: Alex Freidah
//
// Implements integrity.verify_on_replicate. A copy that has just been streamed
// to its target is read back, undone to plaintext, and compared against the
// source row's content_hash before the ledger row that makes it count toward
// the replication factor is written.
//
// Off by default. The read-back doubles the egress a replica costs, which is an
// operator's decision rather than a side effect of enabling hashing.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"log/slog"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// integrityOpReplicate labels this worker's checks in the integrity metrics,
// alongside the read path's "read" and the scrubber's "scrub".
const integrityOpReplicate = "replicate"

// replicaVerdict is what verifyReplica established about a freshly written
// copy. Only replicaMismatch discards the copy: the other three all record it,
// and differ in whether anything was actually proven about it.
type replicaVerdict int

// Verdicts reported by verifyReplica.
const (
	replicaNotChecked replicaVerdict = iota + 1
	replicaVerified
	replicaUnverified
	replicaMismatch
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// admitVerifiedReplica applies the verify-on-replicate verdict to a copy that
// has just landed, tallying it on out. Returns false when the copy was rejected
// and its bytes discarded, in which case the caller must try another target.
func (r *Replicator) admitVerifiedReplica(ctx context.Context, key, target string, source *core.ObjectLocation, out *ReplicationOutcome) bool {
	switch r.verifyReplica(ctx, target, source) {
	case replicaMismatch:
		r.CleanupOrphan(ctx, target, key, source.SizeBytes)
		out.VerifyMismatch++
		return false
	case replicaUnverified:
		out.VerifyUnchecked++
	}
	return true
}

// verifyReplica reads a newly written copy back from its target and compares
// its plaintext digest against the source row's content_hash.
//
// Only a digest that disagrees rejects the copy. Every other outcome - no
// stored hash to compare against, no egress headroom to spend, an unreadable
// copy - reports replicaUnverified and keeps it. A copy that cannot be checked
// is not a copy known to be bad, and discarding it would leave the object
// under-replicated to punish a backend for being slow or a hash for being
// absent. Backends that are not read-after-write consistent make that failure
// mode routine rather than theoretical.
func (r *Replicator) verifyReplica(ctx context.Context, target string, source *core.ObjectLocation) replicaVerdict {
	icfg := r.integrity.Load()
	if icfg == nil || !icfg.ShouldVerifyOnReplicate() {
		return replicaNotChecked
	}

	if source.ContentHash == "" {
		// The object predates integrity, or backfill has not reached it.
		// Reading the copy would produce a digest with nothing to judge it by.
		r.log.WarnContext(ctx, "replica not verified, source has no content hash",
			"key", source.ObjectKey, "source", source.BackendName, "target", target)
		return replicaUnverified
	}

	// StreamCopy moves the stored bytes verbatim, so the source row describes
	// the new copy exactly once the backend name is swapped.
	replica := *source
	replica.BackendName = target

	if !r.ops.Usage().WithinLimits(target, getObjectOp, replica.SizeBytes, 0) {
		telemetry.IntegrityUsageDeclinedTotal.Inc()
		r.log.WarnContext(ctx, "replica verification declined by usage limits",
			"key", source.ObjectKey, "target", target, "size", replica.SizeBytes)
		return replicaUnverified
	}

	actual, err := r.hasher.hashStored(ctx, &replica)
	if err != nil {
		r.log.WarnContext(ctx, "could not verify new replica, recording it unverified",
			"key", source.ObjectKey, "target", target, "error", err)
		return replicaUnverified
	}

	telemetry.IntegrityChecksTotal.WithLabelValues(integrityOpReplicate).Inc()
	if actual.SHA256 == source.ContentHash {
		return replicaVerified
	}

	// Which end is damaged is not knowable from here: the copy is verbatim, so
	// a source that has already rotted produces a faithful copy of bad bytes
	// and every target will disagree the same way. Both names are logged so an
	// operator can settle it with a scrub of the source key.
	telemetry.IntegrityErrorsTotal.WithLabelValues(integrityOpReplicate).Inc()
	r.log.ErrorContext(ctx, "new replica failed integrity check, discarding it",
		"key", source.ObjectKey, "source", source.BackendName, "target", target,
		"expected_hash", source.ContentHash, "actual_hash", actual)
	audit.Log(ctx, "integrity.replica_rejected",
		slog.String("key", source.ObjectKey),
		slog.String("src_backend", source.BackendName),
		slog.String("target_backend", target),
	)
	return replicaMismatch
}
