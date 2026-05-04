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

package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// ReconcileResult holds the outcome of a reconciliation pass for one backend.
type ReconcileResult struct {
	Imported        int `json:"imported"`
	Removed         int `json:"removed"`
	BackendsScanned int `json:"backends_scanned"`
}

// BackendSyncer is the interface the reconciler needs from the proxy layer.
// Defined here to avoid a worker->proxy import cycle.
type BackendSyncer interface {
	SyncBackend(ctx context.Context, backendName, bucket string, knownBuckets []string) (imported, skipped int, err error)
	ReconcileBackend(ctx context.Context, backendName, bucket string, knownBuckets []string) (*ReconcileResult, error)
	UpdateQuotaMetrics(ctx context.Context) error
	BackendOrder() []string
}

// Reconciler scans backends for untracked objects and imports them into the
// metadata database.
type Reconciler struct {
	syncer      BackendSyncer
	bucketNames []string
}

// NewReconciler creates a reconciler that uses the syncer's SyncBackend to
// import untracked objects.
func NewReconciler(syncer BackendSyncer, bucketNames []string) *Reconciler {
	return &Reconciler{
		syncer:      syncer,
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

	for _, backendName := range r.syncer.BackendOrder() {
		bucket := r.bucketNames[0]

		imported, skipped, err := r.syncer.SyncBackend(ctx, backendName, bucket, r.bucketNames)
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
		slog.InfoContext(ctx, "reconcile complete",
			"imported", totalImported, "skipped", totalSkipped,
			"duration", duration.Round(time.Millisecond))

		if err := r.syncer.UpdateQuotaMetrics(ctx); err != nil {
			slog.WarnContext(ctx, "failed to update quota metrics after reconcile", "error", err)
		}
	}

	audit.Log(ctx, "storage.ReconcileComplete",
		slog.Int("imported", totalImported),
		slog.Int("skipped", totalSkipped),
		slog.String("duration", duration.Round(time.Millisecond).String()),
	)
}

// Reconcile performs a full reconciliation for the given backend (or all
// backends if backendName is empty). Lists objects on each backend, diffs
// against DB entries, imports untracked objects, and removes stale entries.
func (r *Reconciler) Reconcile(ctx context.Context, backendName string) (*ReconcileResult, error) {
	ctx = audit.WithRequestID(ctx, audit.NewID())

	if len(r.bucketNames) == 0 {
		return nil, fmt.Errorf("no buckets configured")
	}

	var backends []string
	if backendName != "" {
		backends = []string{backendName}
	} else {
		backends = r.syncer.BackendOrder()
	}

	total := &ReconcileResult{}
	for _, name := range backends {
		result, err := r.syncer.ReconcileBackend(ctx, name, r.bucketNames[0], r.bucketNames)
		if err != nil {
			slog.ErrorContext(ctx, "Reconcile: backend failed", "backend", name, "error", err)
			continue
		}
		total.Imported += result.Imported
		total.Removed += result.Removed
		total.BackendsScanned += result.BackendsScanned
	}

	if total.Imported > 0 || total.Removed > 0 {
		if err := r.syncer.UpdateQuotaMetrics(ctx); err != nil {
			slog.WarnContext(ctx, "failed to update quota metrics after reconcile", "error", err)
		}
	}

	audit.Log(ctx, "storage.ReconcileComplete",
		slog.Int("imported", total.Imported),
		slog.Int("removed", total.Removed),
		slog.Int("backends_scanned", total.BackendsScanned),
	)

	return total, nil
}
