// -------------------------------------------------------------------------------
// DI - Background Worker Providers
//
// Author: Alex Freidah
//
// One Provide<Worker> per background worker. Each provider invokes the
// backend runtime (which satisfies worker.Ops),
// the write coordinator, and the opened store, which already satisfies every
// per-worker store contract via implicit interface satisfaction. The
// resolveWorkerCore / resolveWorkerCoreWithCfg helpers centralize that
// dependency pattern so each provider stays a single short call plus the
// worker-specific constructor arguments.
// -------------------------------------------------------------------------------

package di

import (
	"fmt"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/instanceid"
	"github.com/afreidah/s3-orchestrator/internal/provisioning"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/proxy/multipart"
	"github.com/afreidah/s3-orchestrator/internal/proxy/reconcile"
	"github.com/afreidah/s3-orchestrator/internal/proxy/usage"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// workerCore bundles the dependencies every background worker needs.
type workerCore struct {
	Runtime *infra.BackendRuntime
	Coord   *writepath.Coordinator
	Stores  metadataStore
}

// workerCoreWithCfg extends workerCore with *config.Config for workers
// whose constructors take reloadable config knobs.
type workerCoreWithCfg struct {
	workerCore
	Cfg *config.Config
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// workerCoreFrom pulls the runtime, coordinator and store through an
// in-flight resolver, so a caller that needs more than the core resolves
// everything in one batch. Errors are wrapped with the dependency name so a
// missing provider points at the right registration site.
func workerCoreFrom(r *resolver) workerCore {
	return workerCore{
		Runtime: r.ResolveNamed[*infra.BackendRuntime]("BackendRuntime"),
		Coord:   r.ResolveNamed[*writepath.Coordinator]("WriteCoordinator"),
		Stores:  r.ResolveNamed[metadataStore]("MetadataStore"),
	}
}

// resolveWorkerCore resolves the pair every worker provider depends on.
func resolveWorkerCore(i do.Injector) (workerCore, error) {
	r := newResolver(i)
	return workerCoreFrom(r), r.err
}

// resolveWorkerCoreWithCfg pulls *config.Config first, then the worker
// core, mirroring the historic resolution order. Resolving config first
// lets feature-gated providers short-circuit before touching the runtime.
func resolveWorkerCoreWithCfg(i do.Injector) (workerCoreWithCfg, error) {
	r := newResolver(i)
	cfg := r.ResolveNamed[*config.Config]("Config")
	return workerCoreWithCfg{workerCore: workerCoreFrom(r), Cfg: cfg}, r.err
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// ProvideRebalancer constructs the rebalancer worker.
func ProvideRebalancer(i do.Injector) (*worker.Rebalancer, error) {
	c, err := resolveWorkerCore(i)
	if err != nil {
		return nil, err
	}
	rb := worker.NewRebalancer(c.Runtime, c.Coord, c.Stores)
	rb.SetGaugePublisher(c.Runtime)
	return rb, nil
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
		Ops:       c.Runtime,
		Placement: c.Coord,
		Store:     c.Stores,
		Encryptor: enc,
		Codec:     codec,
	}), nil
}

// resolveStoredForm resolves the two optional decoders a worker needs to turn
// stored bytes back into the plaintext a content hash covers. The codec is
// resolved even with compression disabled, so already-compressed objects stay
// readable.
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
	return worker.NewOverReplicationCleaner(c.Runtime, c.Coord, c.Stores), nil
}

// ProvideCleanupWorker constructs the cleanup-queue worker. It resolves
// the backend runtime and store directly so the drain manager (which
// takes this worker's ProcessCleanupQueue hook) can be built from the
// same pieces without an ordering dependency between the two.
func ProvideCleanupWorker(i do.Injector) (*worker.CleanupWorker, error) {
	r := newResolver(i)
	cfg := r.ResolveNamed[*config.Config]("Config")
	rt := r.ResolveNamed[*infra.BackendRuntime]("BackendRuntime")
	stores := r.ResolveNamed[metadataStore]("MetadataStore")
	id := r.ResolveNamed[instanceid.ID]("InstanceID")
	if r.err != nil {
		return nil, r.err
	}
	concurrency := cfg.CleanupQueue.Concurrency
	if concurrency <= 0 {
		concurrency = 10
	}
	w := worker.NewCleanupWorker(worker.CleanupWorkerDeps{
		Ops:              rt,
		Store:            stores,
		Concurrency:      concurrency,
		InstanceID:       id.String(),
		ClaimGracePeriod: cfg.CleanupQueue.ClaimGracePeriod,
	})
	w.SetGaugePublisher(rt)
	return w, nil
}

// ProvidePendingReaper constructs the pending-reaper worker. It is always
// provided, since every write claims its bytes with an intent. A nil
// dependency here is a wiring bug and surfaces as an error, so
// Optional[*worker.PendingReaper] reports Failed.
func ProvidePendingReaper(i do.Injector) (*worker.PendingReaper, error) {
	c, err := resolveWorkerCoreWithCfg(i)
	if err != nil {
		return nil, err
	}
	if c.Stores == nil {
		return nil, fmt.Errorf("pending reaper: MetadataStore resolved to nil")
	}
	r := worker.NewPendingReaper(worker.PendingReaperDeps{
		Ops:       c.Runtime,
		Placement: c.Coord,
		Store:     c.Stores,
		MinAge:    c.Cfg.WritePath.PendingPattern.MinAge,
		BatchSize: c.Cfg.WritePath.PendingPattern.BatchSize,
	})
	r.SetGaugePublisher(c.Runtime)
	return r, nil
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
	s := worker.NewScrubber(worker.ScrubberDeps{
		Ops:       c.Runtime,
		Placement: c.Coord,
		Store:     c.Stores,
		Encryptor: enc,
		Codec:     codec,
	})
	s.SetGaugePublisher(c.Runtime)
	return s, nil
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
	declared, err := do.Invoke[*provisioning.Declared](i)
	if err != nil {
		return nil, err
	}
	rec, err := do.Invoke[*reconcile.Manager](i)
	if err != nil {
		return nil, err
	}
	usageSvc, err := do.Invoke[*usage.Service](i)
	if err != nil {
		return nil, err
	}
	return worker.NewReconciler(&worker.ReconcilerDeps{
		Syncer:  rec,
		Fleet:   c.Runtime,
		Usage:   usageSvc,
		Buckets: declared,
	}), nil
}

// ProvideDrainManager constructs the drain manager, the operator side of a
// drain, from the backend runtime and the opened store's object, drain and
// backend-lifecycle roles.
func ProvideDrainManager(i do.Injector) (*drain.Manager, error) {
	r := newResolver(i)
	rt := r.ResolveNamed[*infra.BackendRuntime]("BackendRuntime")
	stores := r.ResolveNamed[metadataStore]("MetadataStore")
	if r.err != nil {
		return nil, r.err
	}
	return drain.New(rt, stores, stores, stores), nil
}

// ProvideDrainer constructs the drain worker. It hands every set of drain
// records it reads to the drain manager, which is what keeps IsDraining current
// on the instance that runs it, and aborts a drained backend's open uploads
// through the multipart manager.
func ProvideDrainer(i do.Injector) (*worker.Drainer, error) {
	c, err := resolveWorkerCore(i)
	if err != nil {
		return nil, err
	}
	r := newResolver(i)
	dm := r.Resolve[*drain.Manager]()
	mp := r.ResolveNamed[*multipart.Manager]("MultipartManager")
	if r.err != nil {
		return nil, r.err
	}
	return worker.NewDrainer(worker.DrainerDeps{
		Ops:          c.Runtime,
		Placement:    c.Coord,
		Store:        c.Stores,
		AbortUploads: mp.AbortMultipartUploadsOnBackend,
		OnRecords:    dm.SetStates,
	}), nil
}
