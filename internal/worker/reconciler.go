// -------------------------------------------------------------------------------
// Reconciler - Background Orphan Discovery and Import
//
// Author: Alex Freidah
//
// Periodically scans each backend's S3 bucket and imports untracked objects
// into the metadata database via SyncBackend. Objects the proxy doesn't know
// about (orphans from failed writes, manual uploads, etc.) are brought under
// management so quota accounting stays accurate. An operator's reconcile walks
// the same backends with ReconcileBackend, which also drops ledger rows whose
// objects are gone.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/cycle"
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

// ReconcileResult is one reconcile pass: each backend scanned is an item in
// the tally, and what the scans found is added up. NotImported counts objects
// a scan found outside any declared bucket, and SuppressedPendingCleanup the
// stale rows left alone because their bytes are already queued for cleanup.
type ReconcileResult struct {
	batch.Summary
	Imported                 int
	Removed                  int
	NotImported              int
	SuppressedPendingCleanup int
}

// AuditAttrs adds what the scans found to the tally's attributes.
//
//nolint:gocritic // hugeParam: value receiver, as every cycle.Result has, so a pass can return its zero result alongside an error.
func (r ReconcileResult) AuditAttrs() []slog.Attr {
	return append(r.Summary.AuditAttrs(),
		slog.Int("imported", r.Imported),
		slog.Int("removed", r.Removed),
		slog.Int("not_imported", r.NotImported),
		slog.Int("suppressed_pending_cleanup", r.SuppressedPendingCleanup),
	)
}

// Changed says the pass imported or removed a ledger row.
//
//nolint:gocritic // hugeParam: value receiver, as every cycle.Result has, so a pass can return its zero result alongside an error.
func (r ReconcileResult) Changed() bool {
	return r.Imported > 0 || r.Removed > 0
}

// add folds one backend's scan into the result.
func (r *ReconcileResult) add(s *ReconcileResult) {
	r.Imported += s.Imported
	r.Removed += s.Removed
	r.NotImported += s.NotImported
	r.SuppressedPendingCleanup += s.SuppressedPendingCleanup
}

// BackendSyncer scans and reconciles one backend against the ledger.
// *reconcile.Manager satisfies it.
type BackendSyncer interface {
	SyncBackend(ctx context.Context, backendName, prefix string, knownBuckets []string) (imported, skipped int, err error)
	ReconcileBackend(ctx context.Context, backendName string, knownBuckets []string) (*reconcile.Result, error)
}

// FleetOps is the fleet-wide surface the reconciler walks: the backends to
// visit, and which of them reconcile accepts. *infra.BackendRuntime satisfies
// it.
type FleetOps interface {
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
	d   ReconcilerDeps
	log *slog.Logger
}

// ReconcilerDeps groups what a pass draws on: the diff engine, the fleet it
// walks, the buckets it recognises, the counters it corrects once the diff has
// landed, and the quota gauges it refreshes once a pass changed what they
// report.
type ReconcilerDeps struct {
	Syncer  BackendSyncer
	Fleet   FleetOps
	Usage   UsageReconciler
	Buckets BucketNamer
	Metrics cycle.Refresher
}

// NewReconciler creates a reconciler that uses the syncer's SyncBackend to
// import untracked objects.
func NewReconciler(d *ReconcilerDeps) *Reconciler {
	must.NotNil("d", d)
	must.NotNil("d.Syncer", d.Syncer)
	must.NotNil("d.Fleet", d.Fleet)
	must.NotNil("d.Usage", d.Usage)
	must.NotNil("d.Buckets", d.Buckets)
	must.NotNil("d.Metrics", d.Metrics)
	return &Reconciler{d: *d, log: logfmt.Logger(string(audit.WorkloadReconcile))}
}

// errNoBuckets refuses a pass with no declared bucket to recognise objects by.
var errNoBuckets = errors.New("no buckets declared")

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Run performs the scheduled reconcile: every scannable backend is listed and
// any object the ledger does not track is imported. It then corrects the
// usage counters' drift.
func (r *Reconciler) Run(ctx context.Context) (ReconcileResult, error) {
	pass := cycle.Pass{Workload: audit.WorkloadReconcile, Refresh: r.d.Metrics}
	return cycle.Run(ctx, pass, func(ctx context.Context) (ReconcileResult, error) {
		buckets, err := r.declared()
		if err != nil {
			return ReconcileResult{}, err
		}
		res := r.pass(ctx, r.scannable(), buckets, nil, r.importUntracked)
		r.reconcileUsage(ctx)
		return res, nil
	})
}

// Reconcile performs a full reconciliation for the given backend, or all
// scannable backends if backendName is empty. Lists objects on each backend,
// diffs against the ledger, imports untracked objects, and removes stale
// rows. observer, when non-nil, receives a step per backend.
func (r *Reconciler) Reconcile(ctx context.Context, backendName string, observer progress.Observer) (ReconcileResult, error) {
	buckets, err := r.declared()
	if err != nil {
		return ReconcileResult{}, err
	}
	backends, err := r.backendsFor(backendName)
	if err != nil {
		return ReconcileResult{}, err
	}
	pass := cycle.Pass{Workload: audit.WorkloadReconcile, Refresh: r.d.Metrics}
	return cycle.Run(ctx, pass, func(ctx context.Context) (ReconcileResult, error) {
		return r.pass(ctx, backends, buckets, observer, r.reconcileFully), nil
	})
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// declared returns the buckets a pass recognises objects by, refusing a pass
// when there are none.
func (r *Reconciler) declared() ([]string, error) {
	buckets := r.d.Buckets.Names()
	if len(buckets) == 0 {
		return nil, errNoBuckets
	}
	return buckets, nil
}

// pass scans each backend with scan and adds up what they found. A backend
// whose scan fails is counted failed, keeps what it found before failing, and
// the pass moves on to the next.
func (r *Reconciler) pass(ctx context.Context, backends, buckets []string, observer progress.Observer, scan func(ctx context.Context, name string, buckets []string) (ReconcileResult, error)) ReconcileResult {
	var res ReconcileResult
	runner := batch.Runner[string]{Concurrency: 1, Observer: observer, Key: func(name string) string { return name }}
	res.Summary = runner.Run(ctx, backends, func(ctx context.Context, name string) batch.ItemResult {
		found, err := scan(ctx, name, buckets)
		res.add(&found)
		if err != nil {
			r.log.ErrorContext(ctx, "backend scan failed", "backend", name, "error", err)
			return batch.Failed()
		}
		return batch.Succeeded()
	})
	return res
}

// importUntracked imports the objects on one backend that the ledger does not
// track, and removes nothing.
func (r *Reconciler) importUntracked(ctx context.Context, name string, buckets []string) (ReconcileResult, error) {
	imported, skipped, err := r.d.Syncer.SyncBackend(ctx, name, "", buckets)
	return ReconcileResult{Imported: imported, NotImported: skipped}, err
}

// reconcileFully imports one backend's untracked objects and removes its
// stale rows. A failed reconcile still reports what it did before failing.
func (r *Reconciler) reconcileFully(ctx context.Context, name string, buckets []string) (ReconcileResult, error) {
	found, err := r.d.Syncer.ReconcileBackend(ctx, name, buckets)
	if found == nil {
		return ReconcileResult{}, err
	}
	return ReconcileResult{
		Imported:                 int(found.Imported),
		Removed:                  int(found.Removed),
		SuppressedPendingCleanup: int(found.SuppressedPendingCleanup),
	}, err
}

// reconcileUsage recomputes bytes_used from the object ledger so the
// incrementally maintained counter cannot drift permanently. Runs every pass
// (drift can exist with zero imports); a failure is logged and swallowed so it
// never aborts the reconcile cycle.
func (r *Reconciler) reconcileUsage(ctx context.Context) {
	adjustments, err := r.d.Usage.ReconcileUsage(ctx)
	if err != nil {
		r.log.WarnContext(ctx, "usage reconciliation failed", "error", err)
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
	return r.d.Fleet.Accepted(r.d.Fleet.BackendOrder(), core.AcceptForReconcile)
}

// backendsFor returns the backends an operator-driven reconcile scans: the one
// named, refused when it has a drain record, or every scannable backend.
func (r *Reconciler) backendsFor(backendName string) ([]string, error) {
	if backendName == "" {
		return r.scannable(), nil
	}
	if len(r.d.Fleet.Accepted([]string{backendName}, core.AcceptForReconcile)) == 0 {
		return nil, fmt.Errorf("backend %q has a drain record; reconcile skips it", backendName)
	}
	return []string{backendName}, nil
}
