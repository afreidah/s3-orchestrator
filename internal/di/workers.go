// -------------------------------------------------------------------------------
// DI - Background Worker Providers
//
// Author: Alex Freidah
//
// One Provide<Worker> per background worker. Each provider invokes the
// backend runtime (which satisfies worker.Ops), the write coordinator, the
// metrics collector, and the opened store, which already satisfies every
// per-worker store contract via implicit interface satisfaction, plus the
// running configuration each worker reads its section from. resolveWorkerCore
// centralizes that dependency pattern so each provider stays a single short
// call plus the worker-specific constructor arguments.
// -------------------------------------------------------------------------------

package di

import (
	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/instanceid"
	"github.com/afreidah/s3-orchestrator/internal/provisioning"
	"github.com/afreidah/s3-orchestrator/internal/proxy/backendstate"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/proxy/multipart"
	"github.com/afreidah/s3-orchestrator/internal/proxy/reconcile"
	"github.com/afreidah/s3-orchestrator/internal/proxy/usage"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// workerCore bundles the dependencies every background worker needs: the
// runtime, coordinator, metrics collector and store, and the running
// configuration the workers read their sections from.
type workerCore struct {
	Runtime *infra.BackendRuntime
	Coord   *writepath.Coordinator
	Metrics *metrics.Collector
	Stores  metadataStore
	Live    *config.Live
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// workerCoreFrom pulls the runtime, coordinator, collector, store and running
// config through an in-flight resolver, so a caller that needs more than the
// core resolves everything in one batch.
func workerCoreFrom(r *Resolver) workerCore {
	return workerCore{
		Runtime: r.Resolve[*infra.BackendRuntime](),
		Coord:   r.Resolve[*writepath.Coordinator](),
		Metrics: r.Resolve[*metrics.Collector](),
		Stores:  r.Resolve[metadataStore](),
		Live:    r.Resolve[*config.Live](),
	}
}

// fleet is the fleet every data-moving worker is built over.
func (c *workerCore) fleet() worker.Fleet {
	return worker.Fleet{Ops: c.Runtime, Placement: c.Coord, Metrics: c.Metrics}
}

// resolveWorkerCore resolves the core every worker provider depends on.
func resolveWorkerCore(i do.Injector) (workerCore, error) {
	r := NewResolver(i)
	return workerCoreFrom(r), r.err
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
	return worker.NewRebalancer(worker.RebalancerDeps{Fleet: c.fleet(), Store: c.Stores, Config: c.Live.Rebalance()}), nil
}

// ProvideReplicator constructs the replication worker. It takes the encryptor
// and codec because integrity.verify_on_replicate reads a new copy back, and
// undoing its stored form is what makes the digest comparable to content_hash.
func ProvideReplicator(i do.Injector) (*worker.Replicator, error) {
	c, err := resolveWorkerCore(i)
	if err != nil {
		return nil, err
	}
	enc, codec, err := resolveStoredForm(i, c.Live.Load())
	if err != nil {
		return nil, err
	}
	return worker.NewReplicator(&worker.ReplicatorDeps{
		Fleet:     c.fleet(),
		Store:     c.Stores,
		Config:    c.Live.Replication(),
		Integrity: c.Live.Integrity(),
		Encryptor: enc,
		Codec:     codec,
	}), nil
}

// resolveStoredForm resolves the two optional decoders a worker needs to turn
// stored bytes back into the plaintext a content hash covers. The codec is
// resolved even with compression disabled, so already-compressed objects stay
// readable.
func resolveStoredForm(i do.Injector, cfg *config.Config) (enc *encryption.Encryptor, codec *compression.Codec, err error) {
	if enc, err = resolveOptionalEncryptor(i, cfg.Encryption.Enabled); err != nil {
		return nil, nil, err
	}
	codec, err = do.Invoke[*compression.Codec](i)
	return enc, codec, err
}

// ProvideOverReplicationCleaner constructs the over-replication cleanup worker.
func ProvideOverReplicationCleaner(i do.Injector) (*worker.OverReplicationCleaner, error) {
	c, err := resolveWorkerCore(i)
	if err != nil {
		return nil, err
	}
	return worker.NewOverReplicationCleaner(worker.OverReplicationCleanerDeps{Fleet: c.fleet(), Store: c.Stores, Config: c.Live.Replication()}), nil
}

// ProvideCleanupWorker constructs the cleanup-queue worker. It resolves
// the backend runtime and store directly so the drain manager (which
// takes this worker's ProcessCleanupQueue hook) can be built from the
// same pieces without an ordering dependency between the two.
func ProvideCleanupWorker(i do.Injector) (*worker.CleanupWorker, error) {
	r := NewResolver(i)
	cfg := r.Resolve[*config.Config]()
	rt := r.Resolve[*infra.BackendRuntime]()
	collector := r.Resolve[*metrics.Collector]()
	stores := r.Resolve[metadataStore]()
	id := r.Resolve[instanceid.ID]()
	if r.err != nil {
		return nil, r.err
	}
	return worker.NewCleanupWorker(worker.CleanupWorkerDeps{
		Ops:              rt,
		Metrics:          collector,
		Store:            stores,
		Concurrency:      cfg.CleanupQueue.Concurrency,
		InstanceID:       id.String(),
		ClaimGracePeriod: cfg.CleanupQueue.ClaimGracePeriod,
	}), nil
}

// ProvidePendingReaper constructs the pending-reaper worker. It is always
// provided, since every write claims its bytes with an intent.
func ProvidePendingReaper(i do.Injector) (*worker.PendingReaper, error) {
	c, err := resolveWorkerCore(i)
	if err != nil {
		return nil, err
	}
	pending := c.Live.Load().WritePath.PendingPattern
	return worker.NewPendingReaper(worker.PendingReaperDeps{
		Fleet:     c.fleet(),
		Store:     c.Stores,
		MinAge:    pending.MinAge,
		BatchSize: pending.BatchSize,
	}), nil
}

// ProvideScrubber constructs the integrity-verification worker.
func ProvideScrubber(i do.Injector) (*worker.Scrubber, error) {
	c, err := resolveWorkerCore(i)
	if err != nil {
		return nil, err
	}
	enc, codec, err := resolveStoredForm(i, c.Live.Load())
	if err != nil {
		return nil, err
	}
	return worker.NewScrubber(&worker.ScrubberDeps{
		Fleet:     c.fleet(),
		Store:     c.Stores,
		Config:    c.Live.Integrity(),
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
	r := NewResolver(i)
	rt := r.Resolve[*infra.BackendRuntime]()
	collector := r.Resolve[*metrics.Collector]()
	declared := r.Resolve[*provisioning.Declared]()
	rec := r.Resolve[*reconcile.Manager]()
	usageSvc := r.Resolve[*usage.Service]()
	if r.err != nil {
		return nil, r.err
	}
	return worker.NewReconciler(&worker.ReconcilerDeps{
		Syncer:  rec,
		Fleet:   rt,
		Usage:   usageSvc,
		Buckets: declared,
		Metrics: collector,
	}), nil
}

// ProvideBackendStateManager constructs the backend state manager, the
// operator side of a drain and a backend removal, from the backend runtime and
// the opened store's object and backend-state roles.
func ProvideBackendStateManager(i do.Injector) (*backendstate.Manager, error) {
	r := NewResolver(i)
	rt := r.Resolve[*infra.BackendRuntime]()
	stores := r.Resolve[metadataStore]()
	if r.err != nil {
		return nil, r.err
	}
	return backendstate.New(rt, stores, stores), nil
}

// ProvideDrainer constructs the drain worker. It hands every set of backend
// state records it reads to the backend state manager, which is what keeps
// AcceptsWrites current on the instance that runs it, and aborts a drained
// backend's open uploads through the multipart manager.
func ProvideDrainer(i do.Injector) (*worker.Drainer, error) {
	r := NewResolver(i)
	c := workerCoreFrom(r)
	dm := r.Resolve[*backendstate.Manager]()
	mp := r.Resolve[*multipart.Manager]()
	if r.err != nil {
		return nil, r.err
	}
	return worker.NewDrainer(worker.DrainerDeps{
		Fleet:        c.fleet(),
		Store:        c.Stores,
		AbortUploads: mp.AbortMultipartUploadsOnBackend,
		OnRecords:    dm.SetStates,
	}), nil
}
