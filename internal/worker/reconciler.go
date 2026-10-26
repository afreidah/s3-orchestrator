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
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

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
	SyncBackend(ctx context.Context, backendName string, knownBuckets []string) (imported, skipped int, err error)
	ReconcileBackend(ctx context.Context, backendName string, knownBuckets []string) (*reconcile.Result, error)
}

// FleetOps is the fleet-wide surface the reconciler walks and republishes:
// the backends to visit, which of them reconcile accepts, and the quota gauges
// to refresh once a pass has changed what they report.
// *infra.BackendRuntime satisfies it.
type FleetOps interface {
	UpdateQuotaMetrics(ctx context.Context) error
	BackendOrder() []string
	Accepted(eligible []string, accept core.BackendFlags) []string
}

// UsageReconciler corrects the drift in the incrementally maintained byte
// counters, which a reconcile pass is the natural point to do: it has just
// established what each backend actually holds. *usage.Service satisfies it.
type UsageReconciler interface {
	ReconcileUsage(ctx context.Context) (map[string]int64, error)
}

// BucketNamer lists the virtual buckets a deployment declares, from the config
// file and the store together. *provisioning.Declared satisfies it. It is
// read per pass so newly provisioned buckets are scanned without a restart.
type BucketNamer interface {
	Names() []string
}

// Reconciler scans backends for untracked objects and imports them into the
// metadata database.
type Reconciler struct {
	log     *slog.Logger
	syncer  BackendSyncer
	fleet   FleetOps
	usage   UsageReconciler
	buckets BucketNamer
}

// ReconcilerDeps groups what a pass draws on: the diff engine, the fleet it
// walks, the buckets it recognises, and the counters it corrects once the diff
// has landed.
type ReconcilerDeps struct {
	Syncer  BackendSyncer
	Fleet   FleetOps
	Usage   UsageReconciler
	Buckets BucketNamer
}

// NewReconciler creates a reconciler that uses the syncer's SyncBackend to
// import untracked objects.
func NewReconciler(d *ReconcilerDeps) *Reconciler {
	must.NotNil("d", d)
	must.NotNil("d.Syncer", d.Syncer)
	must.NotNil("d.Fleet", d.Fleet)
	must.NotNil("d.Usage", d.Usage)
	must.NotNil("d.Buckets", d.Buckets)
	return &Reconciler{
		log:     workerLogger(audit.WorkloadReconcile),
		syncer:  d.Syncer,
		fleet:   d.Fleet,
		usage:   d.Usage,
		buckets: d.Buckets,
	}
}

// Run performs a full reconciliation pass: for each backend, list all objects
// and import any that are not tracked in the metadata database.
func (r *Reconciler) Run(ctx context.Context) {
	runTickCycle(ctx, audit.WorkloadReconcile, r.run)
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// reconcileSummary is one scheduled reconcile pass: each backend scanned is an
// item in the tally, and the objects the scans imported and skipped are added.
type reconcileSummary struct {
	batch.Summary
	imported int
	skipped  int
}

// AuditAttrs adds the objects imported and skipped to the tally's attributes.
func (s *reconcileSummary) AuditAttrs() []slog.Attr {
	return append(s.Summary.AuditAttrs(), slog.Int("imported", s.imported), slog.Int("skipped", s.skipped))
}

// run is the body of Run after the span is open. A backend whose scan fails is
// counted failed and the pass moves on to the next.
func (r *Reconciler) run(ctx context.Context) *reconcileSummary {
	start := time.Now()
	sum := &reconcileSummary{}

	bucketNames := r.buckets.Names()
	if len(bucketNames) == 0 {
		r.log.ErrorContext(ctx, "no buckets declared, skipping")
		return sum
	}

	for _, backendName := range r.scannable() {
		sum.Planned++
		imported, skipped, err := r.syncer.SyncBackend(ctx, backendName, bucketNames)
		if err != nil {
			r.log.ErrorContext(ctx, "backend scan failed",
				"backend", backendName, "error", err)
			sum.Failed++
			continue
		}
		sum.Succeeded++
		sum.imported += imported
		sum.skipped += skipped
	}
	sum.Attempted = sum.Succeeded + sum.Failed
	sum.Duration = time.Since(start)

	if sum.imported > 0 {
		r.log.InfoContext(ctx, "reconcile complete",
			"imported", sum.imported, "skipped", sum.skipped,
			"duration", sum.Duration.Round(time.Millisecond))

		if err := r.fleet.UpdateQuotaMetrics(ctx); err != nil {
			r.log.WarnContext(ctx, "failed to update quota metrics after reconcile", "error", err)
		}
	}

	r.reconcileUsage(ctx)
	return sum
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

// scannable returns the backends a reconcile may scan: every backend whose
// flags reconcile accepts. A drain is refused, since importing from it would
// re-add rows the drain is moving off; a cordon is not.
func (r *Reconciler) scannable() []string {
	return r.fleet.Accepted(r.fleet.BackendOrder(), core.AcceptForReconcile)
}

// backendsFor returns the backends an operator-driven reconcile scans: the one
// named, refused when it has a drain record, or every scannable backend.
func (r *Reconciler) backendsFor(backendName string) ([]string, error) {
	if backendName == "" {
		return r.scannable(), nil
	}
	if len(r.fleet.Accepted([]string{backendName}, core.AcceptForReconcile)) == 0 {
		return nil, fmt.Errorf("backend %q has a drain record; reconcile skips it", backendName)
	}
	return []string{backendName}, nil
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Reconcile performs a full reconciliation for the given backend (or all
// backends if backendName is empty). Lists objects on each backend, diffs
// against DB entries, imports untracked objects, and removes stale entries.
// observer, when non-nil, receives a step per backend so a streaming caller can
// report incremental progress.
func (r *Reconciler) Reconcile(ctx context.Context, backendName string, observer progress.Observer) (*ReconcileResult, error) {
	return runOps(ctx, audit.WorkloadReconcile, string(audit.WorkloadReconcile), func(ctx context.Context) (*ReconcileResult, error) {
		return r.reconcile(ctx, backendName, observer)
	})
}

// reconcile is the body of Reconcile after the span is open.
func (r *Reconciler) reconcile(ctx context.Context, backendName string, observer progress.Observer) (*ReconcileResult, error) {
	bucketNames := r.buckets.Names()
	if len(bucketNames) == 0 {
		return nil, fmt.Errorf("no buckets declared")
	}

	backends, err := r.backendsFor(backendName)
	if err != nil {
		return nil, err
	}

	total := &ReconcileResult{}
	for _, name := range backends {
		progress.Track(observer, name, func() string {
			result, err := r.syncer.ReconcileBackend(ctx, name, bucketNames)
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

	audit.Log(ctx, string(audit.WorkloadReconcile)+".complete",
		slog.Int("imported", total.Imported),
		slog.Int("removed", total.Removed),
		slog.Int("suppressed_pending_cleanup", total.SuppressedPendingCleanup),
		slog.Int("backends_scanned", total.BackendsScanned),
	)

	return total, nil
}
