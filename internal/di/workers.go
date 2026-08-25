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
	"context"
	"fmt"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/instanceid"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/proxy/multipart"
	"github.com/afreidah/s3-orchestrator/internal/proxy/reconcile"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// workerCore bundles the dependencies every background worker needs.
type workerCore struct {
	Mgr    *proxy.BackendManager
	Coord  *writepath.Coordinator
	Stores core.MetadataStore
}

// workerCoreWithCfg extends workerCore with *config.Config for workers
// whose constructors take reloadable config knobs.
type workerCoreWithCfg struct {
	workerCore
	Cfg *config.Config
}

// workerCoreFrom pulls the BackendManager + MetadataStore pair through an
// in-flight resolver, so a caller that needs more than the core resolves
// everything in one batch. Errors are wrapped with the dependency name so a
// missing provider points at the right registration site.
func workerCoreFrom(r *resolver) workerCore {
	return workerCore{
		Mgr:    resolveNamed[*proxy.BackendManager](r, "BackendManager"),
		Coord:  resolveNamed[*writepath.Coordinator](r, "WriteCoordinator"),
		Stores: resolveNamed[core.MetadataStore](r, "MetadataStore"),
	}
}

// resolveWorkerCore resolves the pair every worker provider depends on.
func resolveWorkerCore(i do.Injector) (workerCore, error) {
	r := newResolver(i)
	return workerCoreFrom(r), r.err
}

// resolveWorkerCoreWithCfg pulls *config.Config first, then the worker
// core, mirroring the historic resolution order. Resolving config first
// lets feature-gated providers short-circuit before touching the manager.
func resolveWorkerCoreWithCfg(i do.Injector) (workerCoreWithCfg, error) {
	r := newResolver(i)
	cfg := resolveNamed[*config.Config](r, "Config")
	return workerCoreWithCfg{workerCore: workerCoreFrom(r), Cfg: cfg}, r.err
}

// ProvideRebalancer constructs the rebalancer worker.
func ProvideRebalancer(i do.Injector) (*worker.Rebalancer, error) {
	c, err := resolveWorkerCore(i)
	if err != nil {
		return nil, err
	}
	return worker.NewRebalancer(c.Mgr.Runtime(), c.Coord, c.Stores), nil
}

// ProvideReplicator constructs the replication worker. It takes the encryptor
// and codec because integrity.verify_on_replicate reads a new copy back, and
// undoing its stored form is what makes the digest comparable to content_hash.
func ProvideReplicator(i do.Injector) (*worker.Replicator, error) {
	c, err := resolveWorkerCoreWithCfg(i)
	if err != nil {
		return nil, err
	}
	enc, codec, err := resolveStoredForm(i, c.Cfg)
	if err != nil {
		return nil, err
	}
	return worker.NewReplicator(worker.ReplicatorDeps{
		Ops:       c.Mgr.Runtime(),
		Placement: c.Coord,
		Store:     c.Stores,
		Encryptor: enc,
		Codec:     codec,
	}), nil
}

// resolveStoredForm resolves the two optional decoders a worker needs to turn
// stored bytes back into the plaintext a content hash covers.
//
// The codec is resolved whether or not compression is enabled for writes:
// objects already stored compressed still have to be readable after an operator
// turns the feature off.
func resolveStoredForm(i do.Injector, cfg *config.Config) (*encryption.Encryptor, *compression.Codec, error) {
	var enc *encryption.Encryptor
	if cfg.Encryption.Enabled {
		if e, err := do.Invoke[*encryption.Encryptor](i); err == nil {
			enc = e
		}
	}
	codec, err := do.Invoke[*compression.Codec](i)
	if err != nil {
		return nil, nil, err
	}
	return enc, codec, nil
}

// ProvideOverReplicationCleaner constructs the over-replication cleanup worker.
func ProvideOverReplicationCleaner(i do.Injector) (*worker.OverReplicationCleaner, error) {
	c, err := resolveWorkerCore(i)
	if err != nil {
		return nil, err
	}
	return worker.NewOverReplicationCleaner(c.Mgr.Runtime(), c.Coord, c.Stores), nil
}

// ProvideCleanupWorker constructs the cleanup-queue worker. It resolves
// the backend runtime and store directly rather than through the manager
// so the drain manager (which takes this worker's ProcessCleanupQueue
// hook) can be built before the manager.
func ProvideCleanupWorker(i do.Injector) (*worker.CleanupWorker, error) {
	r := newResolver(i)
	cfg := resolveNamed[*config.Config](r, "Config")
	rt := resolveNamed[*infra.BackendRuntime](r, "BackendRuntime")
	stores := resolveNamed[core.MetadataStore](r, "MetadataStore")
	id := resolveNamed[instanceid.ID](r, "InstanceID")
	if r.err != nil {
		return nil, r.err
	}
	concurrency := cfg.CleanupQueue.Concurrency
	if concurrency <= 0 {
		concurrency = 10
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
	c, err := resolveWorkerCoreWithCfg(i)
	if err != nil {
		return nil, err
	}
	if c.Stores == nil {
		return nil, fmt.Errorf("pending pattern enabled but MetadataStore resolved to nil")
	}
	return worker.NewPendingReaper(worker.PendingReaperDeps{
		Ops:       c.Mgr.Runtime(),
		Placement: c.Coord,
		Store:     c.Stores,
		MinAge:    c.Cfg.WritePath.PendingPattern.MinAge,
		BatchSize: c.Cfg.WritePath.PendingPattern.BatchSize,
	}), nil
}

// ProvideScrubber constructs the integrity-verification worker.
func ProvideScrubber(i do.Injector) (*worker.Scrubber, error) {
	c, err := resolveWorkerCoreWithCfg(i)
	if err != nil {
		return nil, err
	}
	enc, codec, err := resolveStoredForm(i, c.Cfg)
	if err != nil {
		return nil, err
	}
	return worker.NewScrubber(worker.ScrubberDeps{
		Ops:       c.Mgr.Runtime(),
		Placement: c.Coord,
		Store:     c.Stores,
		Encryptor: enc,
		Codec:     codec,
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
	rec, err := do.Invoke[*reconcile.Manager](i)
	if err != nil {
		return nil, err
	}
	return worker.NewReconciler(rec, c.Mgr, bktNames), nil
}

// ProvideDrainManager constructs the drain manager from the backend
// runtime (fleet/copy/delete primitives), the write coordinator (its
// mover), the wide MetadataStore for the object/quota/lifecycle role
// surfaces, the multipart manager's abort hook, and the cleanup worker's
// queue flush. None of these is the BackendManager, so drain builds
// before the manager and is injected into it.
func ProvideDrainManager(i do.Injector) (*drain.Manager, error) {
	r := newResolver(i)
	rt := resolveNamed[*infra.BackendRuntime](r, "BackendRuntime")
	coord := resolveNamed[*writepath.Coordinator](r, "WriteCoordinator")
	stores := resolveNamed[core.MetadataStore](r, "MetadataStore")
	mp := resolveNamed[*multipart.Manager](r, "MultipartManager")
	cleanup := resolveNamed[*worker.CleanupWorker](r, "CleanupWorker")
	if r.err != nil {
		return nil, r.err
	}
	// drain wants a (processed, failed) callback; adapt the WorkSummary return
	// so drain stays decoupled from worker.WorkSummary.
	processCleanup := func(ctx context.Context) (int, int) {
		sum := cleanup.ProcessCleanupQueue(ctx)
		return sum.Succeeded, sum.Failed
	}
	return drain.New(
		rt,
		coord,
		stores,
		stores,
		stores,
		mp.AbortMultipartUploadsOnBackend,
		processCleanup,
	), nil
}
