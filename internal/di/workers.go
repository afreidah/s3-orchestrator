// -------------------------------------------------------------------------------
// DI - Background Worker Providers
//
// Author: Alex Freidah
//
// One Provide<Worker> per background worker. Each provider invokes the
// central *proxy.BackendManager (which satisfies worker.Ops / CleanupOps /
// ScrubberOps via promoted backendCore methods plus its own write-path
// helpers) and the wide core.MetadataStore, which already satisfies every
// per-worker store contract via implicit interface satisfaction. Newly
// constructed workers are wired onto BackendManager so backendCore's
// eligibility filters and write paths can reach them.
// -------------------------------------------------------------------------------

package di

import (
	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/instanceid"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// ProvideRebalancer constructs the rebalancer worker.
func ProvideRebalancer(i do.Injector) (*worker.Rebalancer, error) {
	mgr, err := do.Invoke[*proxy.BackendManager](i)
	if err != nil {
		return nil, err
	}
	stores, err := do.Invoke[core.MetadataStore](i)
	if err != nil {
		return nil, err
	}
	return worker.NewRebalancer(mgr, stores), nil
}

// ProvideReplicator constructs the replication worker.
func ProvideReplicator(i do.Injector) (*worker.Replicator, error) {
	mgr, err := do.Invoke[*proxy.BackendManager](i)
	if err != nil {
		return nil, err
	}
	stores, err := do.Invoke[core.MetadataStore](i)
	if err != nil {
		return nil, err
	}
	return worker.NewReplicator(mgr, stores), nil
}

// ProvideOverReplicationCleaner constructs the over-replication cleanup worker.
func ProvideOverReplicationCleaner(i do.Injector) (*worker.OverReplicationCleaner, error) {
	mgr, err := do.Invoke[*proxy.BackendManager](i)
	if err != nil {
		return nil, err
	}
	stores, err := do.Invoke[core.MetadataStore](i)
	if err != nil {
		return nil, err
	}
	return worker.NewOverReplicationCleaner(mgr, stores), nil
}

// ProvideCleanupWorker constructs the cleanup-queue worker.
func ProvideCleanupWorker(i do.Injector) (*worker.CleanupWorker, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	mgr, err := do.Invoke[*proxy.BackendManager](i)
	if err != nil {
		return nil, err
	}
	cleanup, err := do.Invoke[core.MetadataStore](i)
	if err != nil {
		return nil, err
	}
	concurrency := cfg.CleanupQueue.Concurrency
	if concurrency <= 0 {
		concurrency = 10
	}
	id, err := do.Invoke[instanceid.ID](i)
	if err != nil {
		return nil, err
	}
	return worker.NewCleanupWorker(mgr, cleanup, concurrency, id.String(), cfg.CleanupQueue.ClaimGracePeriod), nil
}

// ProvidePendingReaper constructs the pending-reaper worker. Returns nil
// when the pending pattern is disabled (matches the legacy NewBackendManager
// behavior of only attaching a reaper when stores.Pending is non-nil).
func ProvidePendingReaper(i do.Injector) (*worker.PendingReaper, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	if !cfg.WritePath.PendingPattern.IsEnabled() {
		return nil, nil //nolint:nilnil // intentional: nil signals feature off
	}
	mgr, err := do.Invoke[*proxy.BackendManager](i)
	if err != nil {
		return nil, err
	}
	pending, err := do.Invoke[core.MetadataStore](i)
	if err != nil {
		return nil, err
	}
	if pending == nil {
		return nil, nil //nolint:nilnil // store provider returned nil, feature off
	}
	return worker.NewPendingReaper(mgr, pending, 0, cfg.WritePath.PendingPattern.MinAge, cfg.WritePath.PendingPattern.BatchSize), nil
}

// ProvideScrubber constructs the integrity-verification worker.
func ProvideScrubber(i do.Injector) (*worker.Scrubber, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	mgr, err := do.Invoke[*proxy.BackendManager](i)
	if err != nil {
		return nil, err
	}
	integrity, err := do.Invoke[core.MetadataStore](i)
	if err != nil {
		return nil, err
	}
	var enc *encryption.Encryptor
	if cfg.Encryption.Enabled {
		if e, err := do.Invoke[*encryption.Encryptor](i); err == nil {
			enc = e
		}
	}
	return worker.NewScrubber(mgr, integrity, enc), nil
}

// ProvideReconciler constructs the bucket reconciler worker. Registered
// only in worker/all modes because reconciliation is a worker-side
// background task. Returns the reconciler so the lifecycle manager can
// register a service for it; the reconciler is also resolvable directly
// for the admin handler's inspection endpoints.
func ProvideReconciler(i do.Injector) (*worker.Reconciler, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	mgr, err := do.Invoke[*proxy.BackendManager](i)
	if err != nil {
		return nil, err
	}
	bktNames := make([]string, len(cfg.Buckets))
	for idx, b := range cfg.Buckets {
		bktNames[idx] = b.Name
	}
	return worker.NewReconciler(mgr, bktNames), nil
}

// ProvideDrainManager constructs the drain manager. Depends on
// BackendManager (drain.Core seam), the cleanup worker (for the
// cleanup-queue flush before backend deletion), and the wide
// MetadataStore for the object/quota/lifecycle role surfaces. The
// returned manager is wired onto BackendManager by di.WireManager so
// the provider itself stays free of mutation side effects.
func ProvideDrainManager(i do.Injector) (*drain.Manager, error) {
	mgr, err := do.Invoke[*proxy.BackendManager](i)
	if err != nil {
		return nil, err
	}
	cleanup, err := do.Invoke[*worker.CleanupWorker](i)
	if err != nil {
		return nil, err
	}
	stores, err := do.Invoke[core.MetadataStore](i)
	if err != nil {
		return nil, err
	}
	return drain.New(
		mgr,
		stores,
		stores,
		stores,
		mgr.MultipartManager.AbortMultipartUploadsOnBackend,
		cleanup.ProcessCleanupQueue,
	), nil
}
