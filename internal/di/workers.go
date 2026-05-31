// -------------------------------------------------------------------------------
// DI - Background Worker Providers
//
// Author: Alex Freidah
//
// One Provide<Worker> per background worker. Each provider invokes the
// central *proxy.BackendManager (which satisfies worker.Ops / CleanupOps /
// ScrubberOps via promoted backendCore methods plus its own write-path
// helpers) and the wide core.MetadataStore, which already satisfies every
// per-worker store contract via implicit interface satisfaction. The
// resolveWorkerCore / resolveWorkerCoreWithCfg helpers centralize that
// dependency pattern so each provider stays a single short call plus the
// worker-specific constructor arguments.
// -------------------------------------------------------------------------------

package di

import (
	"fmt"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/instanceid"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// workerCore bundles the dependencies every background worker needs.
type workerCore struct {
	Mgr    *proxy.BackendManager
	Stores core.MetadataStore
}

// workerCoreWithCfg extends workerCore with *config.Config for workers
// whose constructors take reloadable config knobs.
type workerCoreWithCfg struct {
	workerCore
	Cfg *config.Config
}

// resolveWorkerCore resolves the BackendManager + MetadataStore pair every
// worker provider depends on. Errors are wrapped with the dependency name
// so a missing provider points at the right registration site.
func resolveWorkerCore(i do.Injector) (workerCore, error) {
	var c workerCore
	mgr, err := do.Invoke[*proxy.BackendManager](i)
	if err != nil {
		return c, fmt.Errorf("resolve BackendManager: %w", err)
	}
	stores, err := do.Invoke[core.MetadataStore](i)
	if err != nil {
		return c, fmt.Errorf("resolve MetadataStore: %w", err)
	}
	c.Mgr = mgr
	c.Stores = stores
	return c, nil
}

// resolveWorkerCoreWithCfg pulls *config.Config first, then the worker
// core, mirroring the historic resolution order. Resolving config first
// lets feature-gated providers short-circuit before touching the manager.
func resolveWorkerCoreWithCfg(i do.Injector) (workerCoreWithCfg, error) {
	var c workerCoreWithCfg
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return c, fmt.Errorf("resolve Config: %w", err)
	}
	core, err := resolveWorkerCore(i)
	if err != nil {
		return c, err
	}
	c.workerCore = core
	c.Cfg = cfg
	return c, nil
}

// ProvideRebalancer constructs the rebalancer worker.
func ProvideRebalancer(i do.Injector) (*worker.Rebalancer, error) {
	c, err := resolveWorkerCore(i)
	if err != nil {
		return nil, err
	}
	return worker.NewRebalancer(c.Mgr, c.Stores), nil
}

// ProvideReplicator constructs the replication worker.
func ProvideReplicator(i do.Injector) (*worker.Replicator, error) {
	c, err := resolveWorkerCore(i)
	if err != nil {
		return nil, err
	}
	return worker.NewReplicator(c.Mgr, c.Stores), nil
}

// ProvideOverReplicationCleaner constructs the over-replication cleanup worker.
func ProvideOverReplicationCleaner(i do.Injector) (*worker.OverReplicationCleaner, error) {
	c, err := resolveWorkerCore(i)
	if err != nil {
		return nil, err
	}
	return worker.NewOverReplicationCleaner(c.Mgr, c.Stores), nil
}

// ProvideCleanupWorker constructs the cleanup-queue worker.
func ProvideCleanupWorker(i do.Injector) (*worker.CleanupWorker, error) {
	c, err := resolveWorkerCoreWithCfg(i)
	if err != nil {
		return nil, err
	}
	concurrency := c.Cfg.CleanupQueue.Concurrency
	if concurrency <= 0 {
		concurrency = 10
	}
	id, err := do.Invoke[instanceid.ID](i)
	if err != nil {
		return nil, fmt.Errorf("resolve InstanceID: %w", err)
	}
	return worker.NewCleanupWorker(c.Mgr, c.Stores, concurrency, id.String(), c.Cfg.CleanupQueue.ClaimGracePeriod), nil
}

// ProvidePendingReaper constructs the pending-reaper worker. This
// provider is registered in NewInjector ONLY when the pending pattern
// is enabled, so reaching this function implies the feature is on
// (#830). Any nil dependency at this point is a wiring bug, not an
// intentional "feature off" signal - it surfaces as an error so
// Optional[*worker.PendingReaper] reports Failed instead of conflating
// it with Disabled.
func ProvidePendingReaper(i do.Injector) (*worker.PendingReaper, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, fmt.Errorf("resolve Config: %w", err)
	}
	c, err := resolveWorkerCore(i)
	if err != nil {
		return nil, err
	}
	if c.Stores == nil {
		return nil, fmt.Errorf("pending pattern enabled but MetadataStore resolved to nil")
	}
	return worker.NewPendingReaper(c.Mgr, c.Stores, 0, cfg.WritePath.PendingPattern.MinAge, cfg.WritePath.PendingPattern.BatchSize), nil
}

// ProvideScrubber constructs the integrity-verification worker.
func ProvideScrubber(i do.Injector) (*worker.Scrubber, error) {
	c, err := resolveWorkerCoreWithCfg(i)
	if err != nil {
		return nil, err
	}
	var enc *encryption.Encryptor
	if c.Cfg.Encryption.Enabled {
		if e, err := do.Invoke[*encryption.Encryptor](i); err == nil {
			enc = e
		}
	}
	return worker.NewScrubber(c.Mgr, c.Stores, enc), nil
}

// ProvideReconciler constructs the bucket reconciler worker. Registered
// only in worker/all modes because reconciliation is a worker-side
// background task. Returns the reconciler so the lifecycle manager can
// register a service for it; the reconciler is also resolvable directly
// for the admin handler's inspection endpoints.
func ProvideReconciler(i do.Injector) (*worker.Reconciler, error) {
	c, err := resolveWorkerCoreWithCfg(i)
	if err != nil {
		return nil, err
	}
	bktNames := make([]string, len(c.Cfg.Buckets))
	for idx, b := range c.Cfg.Buckets {
		bktNames[idx] = b.Name
	}
	return worker.NewReconciler(c.Mgr, bktNames), nil
}

// ProvideDrainManager constructs the drain manager. Depends on
// BackendManager (drain.Core seam), the cleanup worker (for the
// cleanup-queue flush before backend deletion), and the wide
// MetadataStore for the object/quota/lifecycle role surfaces. The
// returned manager is wired onto BackendManager by di.WireManager so
// the provider itself stays free of mutation side effects.
func ProvideDrainManager(i do.Injector) (*drain.Manager, error) {
	c, err := resolveWorkerCore(i)
	if err != nil {
		return nil, err
	}
	cleanup, err := do.Invoke[*worker.CleanupWorker](i)
	if err != nil {
		return nil, fmt.Errorf("resolve CleanupWorker: %w", err)
	}
	return drain.New(
		c.Mgr,
		c.Stores,
		c.Stores,
		c.Stores,
		c.Mgr.Multipart().AbortMultipartUploadsOnBackend,
		cleanup.ProcessCleanupQueue,
	), nil
}
