// -------------------------------------------------------------------------------
// Admin Handler - Consumer-defined Worker Contracts
//
// Author: Alex Freidah
//
// Each worker the admin handler calls is exposed here as a small consumer-
// defined interface. The handler depends on these interfaces instead of the
// concrete *worker.X types, so the admin transport package does not import
// internal/worker. *worker.Replicator, *worker.OverReplicationCleaner,
// *worker.Scrubber, and *worker.Reconciler each satisfy the matching
// interface via implicit interface satisfaction.
// -------------------------------------------------------------------------------

package admin

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// ReplicatorOps is the slice of *worker.Replicator the admin handler uses
// for the synchronous replicate-now endpoint. Config returns nil when the
// worker is unconfigured; Replicate runs one cycle and returns the count.
type ReplicatorOps interface {
	Config() *config.ReplicationConfig
	Replicate(ctx context.Context, cfg config.ReplicationConfig) (int, error)
}

// OverReplicationOps is the slice of *worker.OverReplicationCleaner the
// admin handler uses for the over-replication status and cleanup endpoints.
type OverReplicationOps interface {
	Config() *config.ReplicationConfig
	CountPending(ctx context.Context, factor int) (int64, error)
	Clean(ctx context.Context, cfg config.ReplicationConfig) (int, error)
}

// ScrubberOps is the slice of *worker.Scrubber the admin handler uses for
// the integrity-scrub and hash-backfill endpoints.
type ScrubberOps interface {
	Scrub(ctx context.Context, batchSize int) (checked, failed int)
	Backfill(ctx context.Context, batchSize, offset int) (processed, nextOffset int)
}

// Reconciler is the slice of *worker.Reconciler the admin handler uses
// for the on-demand reconciliation endpoint.
type Reconciler interface {
	Reconcile(ctx context.Context, backendName string) (*worker.ReconcileResult, error)
}
