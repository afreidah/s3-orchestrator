// -------------------------------------------------------------------------------
// Reconciler - Background Orphan Discovery and Import
//
// Author: Alex Freidah
//
// Periodically scans each backend's S3 bucket and imports untracked objects
// into the metadata database via SyncBackend. Objects the proxy doesn't know
// about (orphans from failed writes, manual uploads, etc.) are brought under
// management so quota accounting stays accurate.
// -------------------------------------------------------------------------------

package storage

import (
	"context"
	"log/slog"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/audit"
	"github.com/afreidah/s3-orchestrator/internal/telemetry"
)

// Reconciler scans backends for untracked objects and imports them into the
// metadata database.
type Reconciler struct {
	manager      *BackendManager
	bucketNames  []string // configured virtual bucket names
}

// NewReconciler creates a reconciler that uses the manager's SyncBackend to
// import untracked objects.
func NewReconciler(manager *BackendManager, bucketNames []string) *Reconciler {
	return &Reconciler{
		manager:     manager,
		bucketNames: bucketNames,
	}
}

// Run performs a full reconciliation pass: for each backend, list all objects
// and import any that are not tracked in the metadata database.
func (r *Reconciler) Run(ctx context.Context) {
	start := time.Now()
	ctx, span := telemetry.StartSpan(ctx, "Reconcile",
		telemetry.AttrOperation.String("reconcile"),
	)
	defer span.End()

	if len(r.bucketNames) == 0 {
		slog.ErrorContext(ctx, "Reconcile: no buckets configured, skipping")
		return
	}

	var totalImported, totalSkipped int

	for _, backendName := range r.manager.order {
		// Use the first bucket as the default prefix for untracked objects.
		// SyncBackend handles objects that already have a bucket prefix.
		bucket := r.bucketNames[0]

		imported, skipped, err := r.manager.SyncBackend(ctx, backendName, bucket, r.bucketNames)
		if err != nil {
			slog.ErrorContext(ctx, "Reconcile: backend scan failed",
				"backend", backendName, "error", err)
			continue
		}
		totalImported += imported
		totalSkipped += skipped
	}

	duration := time.Since(start)

	if totalImported > 0 {
		slog.InfoContext(ctx, "Reconcile complete",
			"imported", totalImported, "skipped", totalSkipped,
			"duration", duration.Round(time.Millisecond))

		// Refresh quota metrics to reflect newly imported objects
		if err := r.manager.UpdateQuotaMetrics(ctx); err != nil {
			slog.WarnContext(ctx, "Failed to update quota metrics after reconcile", "error", err)
		}
	}

	audit.Log(ctx, "storage.ReconcileComplete",
		slog.Int("imported", totalImported),
		slog.Int("skipped", totalSkipped),
		slog.String("duration", duration.Round(time.Millisecond).String()),
	)
}
