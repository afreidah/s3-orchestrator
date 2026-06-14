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
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/proxy/multipart"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// Error-wrap formats shared by the worker dependency-resolution helpers so
// the wrapped message stays identical across providers.
const (
	errResolveConfig        = "resolve Config: %w"
	errResolveRuntime       = "resolve BackendRuntime: %w"
	errResolveMetadataStore = "resolve MetadataStore: %w"
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
		return c, fmt.Errorf(errResolveMetadataStore, err)
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
		return c, fmt.Errorf(errResolveConfig, err)
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
	return worker.NewRebalancer(c.Mgr.Runtime(), c.Mgr, c.Stores), nil
}

// ProvideReplicator constructs the replication worker.
func ProvideReplicator(i do.Injector) (*worker.Replicator, error) {
	c, err := resolveWorkerCore(i)
	if err != nil {
		return nil, err
	}
	return worker.NewReplicator(c.Mgr.Runtime(), c.Mgr, c.Stores), nil
}

// ProvideOverReplicationCleaner constructs the over-replication cleanup worker.
func ProvideOverReplicationCleaner(i do.Injector) (*worker.OverReplicationCleaner, error) {
	c, err := resolveWorkerCore(i)
	if err != nil {
		return nil, err
	}
	return worker.NewOverReplicationCleaner(c.Mgr.Runtime(), c.Mgr, c.Stores), nil
}

// ProvideCleanupWorker constructs the cleanup-queue worker. It resolves
// the backend runtime and store directly rather than through the manager
// so the drain manager (which takes this worker's ProcessCleanupQueue
// hook) can be built before the manager.
func ProvideCleanupWorker(i do.Injector) (*worker.CleanupWorker, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, fmt.Errorf(errResolveConfig, err)
	}
	rt, err := do.Invoke[*infra.BackendRuntime](i)
	if err != nil {
		return nil, fmt.Errorf(errResolveRuntime, err)
	}
	stores, err := do.Invoke[core.MetadataStore](i)
	if err != nil {
		return nil, fmt.Errorf(errResolveMetadataStore, err)
	}
	concurrency := cfg.CleanupQueue.Concurrency
	if concurrency <= 0 {
		concurrency = 10
	}
	id, err := do.Invoke[instanceid.ID](i)
	if err != nil {
		return nil, fmt.Errorf("resolve InstanceID: %w", err)
	}
	return worker.NewCleanupWorker(worker.CleanupWorkerDeps{
		Ops:              rt,
		Store:            stores,
		Concurrency:      concurrency,
		InstanceID:       id.String(),
		ClaimGracePeriod: cfg.CleanupQueue.ClaimGracePeriod,
	}), nil
}

// ProvidePendingReaper constructs the pending-reaper worker. The
// provider is registered in NewInjector only when the pending pattern
// is enabled, so reaching this function implies the feature is on.
// Any nil dependency at this point is a wiring bug, not a "feature
// off" signal — it surfaces as an error so Optional[*worker.PendingReaper]
// reports Failed instead of conflating it with Disabled.
func ProvidePendingReaper(i do.Injector) (*worker.PendingReaper, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, fmt.Errorf(errResolveConfig, err)
	}
	c, err := resolveWorkerCore(i)
	if err != nil {
		return nil, err
	}
	if c.Stores == nil {
		return nil, fmt.Errorf("pending pattern enabled but MetadataStore resolved to nil")
	}
	return worker.NewPendingReaper(worker.PendingReaperDeps{
		Ops:       c.Mgr.Runtime(),
		Placement: c.Mgr,
		Store:     c.Stores,
		MinAge:    cfg.WritePath.PendingPattern.MinAge,
		BatchSize: cfg.WritePath.PendingPattern.BatchSize,
	}), nil
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
	return worker.NewScrubber(worker.ScrubberDeps{
		Ops:       c.Mgr.Runtime(),
		Placement: c.Mgr,
		Store:     c.Stores,
		Encryptor: enc,
	}), nil
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

// ProvideDrainManager constructs the drain manager from the backend
// runtime (fleet/copy/delete primitives), the write coordinator (its
// mover), the wide MetadataStore for the object/quota/lifecycle role
// surfaces, the multipart manager's abort hook, and the cleanup worker's
// queue flush. None of these is the BackendManager, so drain builds
// before the manager and is injected into it.
func ProvideDrainManager(i do.Injector) (*drain.Manager, error) {
	rt, err := do.Invoke[*infra.BackendRuntime](i)
	if err != nil {
		return nil, fmt.Errorf(errResolveRuntime, err)
	}
	coord, err := do.Invoke[*writepath.Coordinator](i)
	if err != nil {
		return nil, fmt.Errorf("resolve WriteCoordinator: %w", err)
	}
	stores, err := do.Invoke[core.MetadataStore](i)
	if err != nil {
		return nil, fmt.Errorf(errResolveMetadataStore, err)
	}
	mp, err := do.Invoke[*multipart.Manager](i)
	if err != nil {
		return nil, fmt.Errorf("resolve MultipartManager: %w", err)
	}
	cleanup, err := do.Invoke[*worker.CleanupWorker](i)
	if err != nil {
		return nil, fmt.Errorf("resolve CleanupWorker: %w", err)
	}
	return drain.New(
		rt,
		coord,
		stores,
		stores,
		stores,
		mp.AbortMultipartUploadsOnBackend,
		cleanup.ProcessCleanupQueue,
	), nil
}
