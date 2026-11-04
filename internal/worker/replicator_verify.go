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
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// admitVerifiedReplica checks a copy that has just landed at intent's path
// against its source, when verification on replicate is on, and tallies the
// outcome on out. Only a disagreeing digest rejects the copy: its intent is
// discarded and false returned, so the caller tries another target. A copy
// that could not be checked is recorded, since it is not known to be bad;
// backends without read-after-write consistency make unreadable copies
// routine.
func (r *Replicator) admitVerifiedReplica(ctx context.Context, intent *core.PendingObject, source *core.ObjectLocation, out *replicationOutcome) bool {
	if !r.d.Integrity.Config().ShouldVerifyOnReplicate() {
		return true
	}
	// StreamCopy moves bytes verbatim, so the source row describes the new
	// copy once the backend and path are swapped.
	replica := source.CopyAt(intent.BackendName, intent.StorageKey)
	switch r.hasher.verify(ctx, replica, source, integrityOpReplicate, r.hasher.admitsRead) {
	case CopyVerified:
		return true
	case CopyMismatch:
		// Which end is damaged is not knowable from here: a source that has
		// already rotted produces a faithful copy of bad bytes. Both backends
		// are named so an operator can settle it with a scrub of the key.
		audit.Log(ctx, "integrity.replica_rejected",
			slog.String("key", source.ObjectKey),
			slog.String("src_backend", source.BackendName),
			slog.String("target_backend", intent.BackendName),
		)
		r.d.Placement.Discard(ctx, intent, writepath.ReplicationReasons.Orphan)
		out.VerifyMismatch++
		return false
	default:
		out.VerifyUnchecked++
		return true
	}
}
