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
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
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
	ReconcileUsage(ctx context.Context) (map[string]int64, error)
	BackendOrder() []string
}

// Reconciler scans backends for untracked objects and imports them into the
// metadata database.
type Reconciler struct {
	log         *slog.Logger
	syncer      BackendSyncer
	bucketNames []string
}

// NewReconciler creates a reconciler that uses the syncer's SyncBackend to
// import untracked objects.
func NewReconciler(syncer BackendSyncer, bucketNames []string) *Reconciler {
	must.NotNil("syncer", syncer)
	return &Reconciler{
		log:         slog.Default().With(logfmt.Component("reconciler")),
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
		r.log.ErrorContext(ctx, "no buckets configured, skipping")
		return
	}

	var totalImported, totalSkipped int

	for _, backendName := range r.syncer.BackendOrder() {
		bucket := r.bucketNames[0]

		imported, skipped, err := r.syncer.SyncBackend(ctx, backendName, bucket, r.bucketNames)
		if err != nil {
			r.log.ErrorContext(ctx, "backend scan failed",
				"backend", backendName, "error", err)
			continue
		}
		totalImported += imported
		totalSkipped += skipped
	}

	duration := time.Since(start)

	if totalImported > 0 {
		r.log.InfoContext(ctx, "reconcile complete",
			"imported", totalImported, "skipped", totalSkipped,
			"duration", duration.Round(time.Millisecond))

		if err := r.syncer.UpdateQuotaMetrics(ctx); err != nil {
			r.log.WarnContext(ctx, "failed to update quota metrics after reconcile", "error", err)
		}
	}

	r.reconcileUsage(ctx)

	audit.Log(ctx, "storage.ReconcileComplete",
		slog.Int("imported", totalImported),
		slog.Int("skipped", totalSkipped),
		slog.String("duration", duration.Round(time.Millisecond).String()),
	)
}

// reconcileUsage recomputes bytes_used from the object ledger so the
// incrementally maintained counter cannot drift permanently. Runs every pass
// (drift can exist with zero imports); a failure is logged and swallowed so it
// never aborts the reconcile cycle.
func (r *Reconciler) reconcileUsage(ctx context.Context) {
	adjustments, err := r.syncer.ReconcileUsage(ctx)
	if err != nil {
		r.log.WarnContext(ctx, "usage reconciliation failed", logfmt.Err(err))
		return
	}
	if len(adjustments) == 0 {
		return
	}
	telemetry.UsageReconcileCorrectionsTotal.Add(float64(len(adjustments)))
	r.log.InfoContext(ctx, "usage reconciliation corrected drift",
		"backends_corrected", len(adjustments))
	audit.Log(ctx, "usage.reconcile", slog.Int("backends_corrected", len(adjustments)))
}

// Reconcile performs a full reconciliation for the given backend (or all
// backends if backendName is empty). Lists objects on each backend, diffs
// against DB entries, imports untracked objects, and removes stale entries.
func (r *Reconciler) Reconcile(ctx context.Context, backendName string) (*ReconcileResult, error) {
	return r.ReconcileStreaming(ctx, backendName, nil)
}

// ReconcileStreaming is Reconcile with a per-backend observer. onBackend, when
// non-nil, is called after each backend is reconciled so a streaming caller can
// report incremental progress; pass nil for the non-streaming path.
func (r *Reconciler) ReconcileStreaming(ctx context.Context, backendName string, observer progress.Observer) (*ReconcileResult, error) {
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
		progress.Track(observer, name, func() string {
			result, err := r.syncer.ReconcileBackend(ctx, name, r.bucketNames[0], r.bucketNames)
			if err != nil {
				r.log.ErrorContext(ctx, "backend failed", "backend", name, "error", err)
				return progress.StatusFailed
			}
			total.Imported += result.Imported
			total.Removed += result.Removed
			total.BackendsScanned += result.BackendsScanned
			return progress.StatusOK
		})
	}

	if total.Imported > 0 || total.Removed > 0 {
		if err := r.syncer.UpdateQuotaMetrics(ctx); err != nil {
			r.log.WarnContext(ctx, "failed to update quota metrics after reconcile", "error", err)
		}
	}

	audit.Log(ctx, "storage.ReconcileComplete",
		slog.Int("imported", total.Imported),
		slog.Int("removed", total.Removed),
		slog.Int("backends_scanned", total.BackendsScanned),
	)

	return total, nil
}
