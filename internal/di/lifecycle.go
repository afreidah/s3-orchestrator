// -------------------------------------------------------------------------------
// DI - Lifecycle Manager Provider
//
// Author: Alex Freidah
//
// Wires lifecycle.Manager and registers a Runner for every background
// service that should start at boot. The Runner constructors themselves
// live in services.go; this file is just the DI glue that resolves the
// worker dependencies and assembles the service list per mode.
// -------------------------------------------------------------------------------

package di

import (
	"context"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	objcache "github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/debug"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/notify"
	"github.com/afreidah/s3-orchestrator/internal/proxy/backendstate"
	"github.com/afreidah/s3-orchestrator/internal/proxy/expiry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/proxy/multipart"
	"github.com/afreidah/s3-orchestrator/internal/proxy/usage"
	"github.com/afreidah/s3-orchestrator/internal/sharedstate"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// lifecycleWorkerSet bundles the workers ProvideLifecycleManager registers
// services for. Resolved via resolveLifecycleWorkers so the provider body
// stays a flat sequence of registrations rather than a chain of error
// returns.
type lifecycleWorkerSet struct {
	cleanup       *worker.CleanupWorker
	rebalancer    *worker.Rebalancer
	replicator    *worker.Replicator
	overRep       *worker.OverReplicationCleaner
	scrubber      *worker.Scrubber
	drainer       *worker.Drainer
	pendingReaper *worker.PendingReaper
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// resolveLifecycleWorkers invokes every worker the lifecycle manager
// registers a service for. backendstate.Manager is not among them:
// di.WireManager owns that resolution and runs first, as part of the runtime's
// startup, so the runtime already knows about the backend state manager by the
// time the lifecycle manager assembles its service list.
func resolveLifecycleWorkers(i do.Injector) (lifecycleWorkerSet, error) {
	r := NewResolver(i)
	ws := lifecycleWorkerSet{
		cleanup:       r.Resolve[*worker.CleanupWorker](),
		rebalancer:    r.Resolve[*worker.Rebalancer](),
		replicator:    r.Resolve[*worker.Replicator](),
		overRep:       r.Resolve[*worker.OverReplicationCleaner](),
		scrubber:      r.Resolve[*worker.Scrubber](),
		drainer:       r.Resolve[*worker.Drainer](),
		pendingReaper: r.Resolve[*worker.PendingReaper](),
	}
	return ws, r.err
}

// registerWorkerServices registers the worker-mode lifecycle services
// (multipart cleanup, cleanup queue, pending reaper, rebalancer,
// replicator, over-replication, drain, lifecycle, scrubber) on sm. The
// scrubber is left out when its interval turns the sweep off.
func registerWorkerServices(sm *lifecycle.Manager, mp *multipart.Manager, expirer *expiry.Manager, ws lifecycleWorkerSet, locker core.AdvisoryLocker, cfg *config.Config) {
	sm.Register(multipart.NewCleanupService(mp, locker, cfg.CleanupQueue.MultipartStaleTimeout))
	sm.Register(worker.NewCleanupQueueService(ws.cleanup, locker))
	sm.Register(worker.NewPendingReaperService(ws.pendingReaper, locker, cfg.WritePath.PendingPattern.ReaperTick))
	sm.Register(worker.NewRebalancerService(ws.rebalancer, locker))
	sm.Register(worker.NewReplicatorService(ws.replicator, locker))
	sm.Register(worker.NewOverReplicationService(ws.overRep, locker))
	sm.Register(worker.NewDrainerService(ws.drainer, locker))
	sm.Register(NewLifecycleService(expirer, locker))
	if cfg.Integrity.ScrubberInterval > 0 {
		sm.Register(worker.NewScrubberService(ws.scrubber, locker))
	}
}

// registerSharedStateWatchers registers the services that apply what other
// instances announce. Every mode holds a provisioning view: API instances
// authenticate against it, workers read its declared buckets. The cache
// watcher runs wherever an object cache is configured.
func registerSharedStateWatchers(sm *lifecycle.Manager, i do.Injector) error {
	shared, err := do.Invoke[*sharedstate.Shared](i)
	if err != nil {
		return err
	}
	sm.Register(newProvisioningWatcher(shared, func(ctx context.Context) error {
		return applyLiveProvisioning(ctx, i)
	}))
	objectCache := Optional[objcache.ObjectCache](i)
	if objectCache.Failed() {
		return objectCache.Err
	}
	if objectCache.Value != nil {
		sm.Register(newCacheWatcher(shared, objectCache.Value))
	}
	return nil
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// ProvideLifecycleManager creates and registers all background services.
func ProvideLifecycleManager(i do.Injector) (*lifecycle.Manager, error) {
	r := NewResolver(i)
	cfg := r.Resolve[*config.Config]()
	live := r.Resolve[*config.Live]()
	rt := r.Resolve[*infra.BackendRuntime]()
	collector := r.Resolve[*metrics.Collector]()
	multipartManager := r.Resolve[*multipart.Manager]()
	usageSvc := r.Resolve[*usage.Service]()
	registry := r.Resolve[*breaker.Registry]()
	locker := r.Resolve[core.AdvisoryLocker]()
	states := r.Resolve[*backendstate.Manager]()
	mode := r.Resolve[config.Mode]()
	if r.err != nil {
		return nil, r.err
	}

	ctx := context.Background()
	sm := lifecycle.NewManager()
	sm.Register(NewUsageFlushService(&UsageFlushDeps{
		Flusher: usageSvc,
		Tracker: rt.Usage(),
		Fleet:   collector,
		States:  states,
		Config:  live.UsageFlush(),
	}))
	sm.Register(NewFleetSnapshotService(collector, locker, cfg.Telemetry.Metrics.FleetInterval))
	sm.Register(breaker.NewWatchdog(registry))
	if fr := Optional[*debug.FlightRecorderService](i).Get(ctx); fr != nil {
		sm.Register(fr)
	}
	if cfg.Redis != nil {
		if err := registerSharedStateWatchers(sm, i); err != nil {
			return nil, err
		}
	}

	if !mode.IsWorker() {
		return sm, nil
	}

	ws, err := resolveLifecycleWorkers(i)
	if err != nil {
		return nil, err
	}
	expirer, err := do.Invoke[*expiry.Manager](i)
	if err != nil {
		return nil, err
	}
	registerWorkerServices(sm, multipartManager, expirer, ws, locker, cfg)

	if cfg.Reconcile.Enabled {
		reconciler, err := do.Invoke[*worker.Reconciler](i)
		if err != nil {
			return nil, err
		}
		sm.Register(worker.NewReconcileService(reconciler, locker, cfg.Reconcile.Interval))
	}
	if notifier := Optional[*notify.Notifier](i).Get(ctx); notifier != nil {
		sm.Register(notifier)
	}

	return sm, nil
}
