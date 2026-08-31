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
	"github.com/afreidah/s3-orchestrator/internal/proxy/reconcile"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
)

// ReconcileResult holds the outcome of a reconciliation pass for one backend.
type ReconcileResult struct {
	Imported                 int `json:"imported"`
	Removed                  int `json:"removed"`
	SuppressedPendingCleanup int `json:"suppressed_pending_cleanup"`
	BackendsScanned          int `json:"backends_scanned"`
}

// BackendSyncer scans and reconciles one backend against the ledger.
// *reconcile.Manager satisfies it.
type BackendSyncer interface {
	SyncBackend(ctx context.Context, backendName, bucket string, knownBuckets []string) (imported, skipped int, err error)
	ReconcileBackend(ctx context.Context, backendName string, knownBuckets []string) (*reconcile.Result, error)
}

// FleetOps is the fleet-wide surface the reconciler walks and republishes:
// the backends to visit, and the quota gauges to refresh once a pass has
// changed what they report. *infra.BackendRuntime satisfies it.
type FleetOps interface {
	UpdateQuotaMetrics(ctx context.Context) error
	BackendOrder() []string
}

// UsageReconciler corrects the drift in the incrementally maintained byte
// counters, which a reconcile pass is the natural point to do: it has just
// established what each backend actually holds. *usage.Service satisfies it.
type UsageReconciler interface {
	ReconcileUsage(ctx context.Context) (map[string]int64, error)
}

// Reconciler scans backends for untracked objects and imports them into the
// metadata database.
type Reconciler struct {
	log         *slog.Logger
	syncer      BackendSyncer
	fleet       FleetOps
	usage       UsageReconciler
	bucketNames []string
}

// ReconcilerDeps groups what a pass draws on: the diff engine, the fleet it
// walks, and the counters it corrects once the diff has landed.
type ReconcilerDeps struct {
	Syncer      BackendSyncer
	Fleet       FleetOps
	Usage       UsageReconciler
	BucketNames []string
}

// NewReconciler creates a reconciler that uses the syncer's SyncBackend to
// import untracked objects.
func NewReconciler(d *ReconcilerDeps) *Reconciler {
	must.NotNil("d", d)
	must.NotNil("d.Syncer", d.Syncer)
	must.NotNil("d.Fleet", d.Fleet)
	must.NotNil("d.Usage", d.Usage)
	return &Reconciler{
		log:         slog.Default().With(logfmt.Component("reconciler")),
		syncer:      d.Syncer,
		fleet:       d.Fleet,
		usage:       d.Usage,
		bucketNames: d.BucketNames,
	}
}

// Run performs a full reconciliation pass: for each backend, list all objects
// and import any that are not tracked in the metadata database.
func (r *Reconciler) Run(ctx context.Context) {
	runTickCycle(ctx, "Reconcile", "reconcile", func(ctx context.Context) struct{} {
		r.run(ctx)
		return struct{}{}
	})
}

// run is the body of Run after the span is open.
func (r *Reconciler) run(ctx context.Context) {
	start := time.Now()

	if len(r.bucketNames) == 0 {
		r.log.ErrorContext(ctx, "no buckets configured, skipping")
		return
	}

	var totalImported, totalSkipped int

	for _, backendName := range r.fleet.BackendOrder() {
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

		if err := r.fleet.UpdateQuotaMetrics(ctx); err != nil {
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
	adjustments, err := r.usage.ReconcileUsage(ctx)
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
		backends = r.fleet.BackendOrder()
	}

	total := &ReconcileResult{}
	for _, name := range backends {
		progress.Track(observer, name, func() string {
			result, err := r.syncer.ReconcileBackend(ctx, name, r.bucketNames)
			// A failed pass still reports what it managed before erroring, so
			// partial progress is not lost from the tally.
			if result != nil {
				total.Imported += int(result.Imported)
				total.Removed += int(result.Removed)
				total.SuppressedPendingCleanup += int(result.SuppressedPendingCleanup)
				total.BackendsScanned++
			}
			if err != nil {
				r.log.ErrorContext(ctx, "backend failed", "backend", name, "error", err)
				return progress.StatusFailed
			}
			return progress.StatusOK
		})
	}

	if total.Imported > 0 || total.Removed > 0 {
		if err := r.fleet.UpdateQuotaMetrics(ctx); err != nil {
			r.log.WarnContext(ctx, "failed to update quota metrics after reconcile", "error", err)
		}
	}

	audit.Log(ctx, "storage.ReconcileComplete",
		slog.Int("imported", total.Imported),
		slog.Int("removed", total.Removed),
		slog.Int("suppressed_pending_cleanup", total.SuppressedPendingCleanup),
		slog.Int("backends_scanned", total.BackendsScanned),
	)

	return total, nil
}
