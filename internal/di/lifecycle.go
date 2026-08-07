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
	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/debug"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/notify"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/expiry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/multipart"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

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
	pendingReaper *worker.PendingReaper // nil when the pending pattern is off
}

// resolveLifecycleWorkers invokes every worker the lifecycle manager
// registers a service for. drain.Manager is no longer invoked here
// because di.WireManager owns that resolution; the wiring step runs
// before resolveLifecycleWorkers as part of cli/serve's startup so
// the drain manager is already installed on BackendManager by the
// time the lifecycle manager assembles its service list.
func resolveLifecycleWorkers(i do.Injector) (lifecycleWorkerSet, error) {
	r := newResolver(i)
	ws := lifecycleWorkerSet{
		cleanup:    resolve[*worker.CleanupWorker](r),
		rebalancer: resolve[*worker.Rebalancer](r),
		replicator: resolve[*worker.Replicator](r),
		overRep:    resolve[*worker.OverReplicationCleaner](r),
		scrubber:   resolve[*worker.Scrubber](r),
	}
	if r.err != nil {
		return ws, r.err
	}
	// PendingReaper is conditionally registered: the provider is
	// only present when cfg.WritePath.PendingPattern.IsEnabled() is
	// true. do.Invoke returns an error for both "not registered" (
	// feature off) and "registered but constructor failed". WireManager
	// has already logged the Failed case via Optional[*worker.PendingReaper];
	// here we just take the nil value either way.
	ws.pendingReaper, _ = do.Invoke[*worker.PendingReaper](i)
	return ws, nil
}

// registerWorkerServices registers the worker-mode lifecycle services
// (multipart cleanup, cleanup queue, pending reaper, rebalancer,
// replicator, over-replication, lifecycle, scrubber) on sm.
func registerWorkerServices(sm *lifecycle.Manager, mgr *proxy.BackendManager, expirer *expiry.Manager, ws lifecycleWorkerSet, locker core.AdvisoryLocker, cfg *config.Config) {
	sm.Register("multipart-cleanup", multipart.NewCleanupService(mgr.Multipart(), locker, cfg.CleanupQueue.MultipartStaleTimeout))
	sm.Register("cleanup-queue", worker.NewCleanupQueueService(ws.cleanup, locker))
	if svc := worker.NewPendingReaperService(ws.pendingReaper, locker, cfg.WritePath.PendingPattern.ReaperTick); svc != nil {
		sm.Register("pending-reaper", svc)
	}
	sm.Register("rebalancer", worker.NewRebalancerService(mgr.Runtime(), ws.rebalancer, locker))
	sm.Register("replicator", worker.NewReplicatorService(mgr.Runtime(), ws.replicator, locker))
	sm.Register("over-replication", worker.NewOverReplicationService(mgr.Runtime(), ws.overRep, locker))
	sm.Register("lifecycle", NewLifecycleService(expirer, locker))
	sm.Register("scrubber", worker.NewScrubberService(ws.scrubber, locker))
}

// ProvideLifecycleManager creates and registers all background services.
func ProvideLifecycleManager(i do.Injector) (*lifecycle.Manager, error) {
	r := newResolver(i)
	cfg := resolve[*config.Config](r)
	manager := resolve[*proxy.BackendManager](r)
	registry := resolve[*breaker.Registry](r)
	locker := resolve[core.MetadataStore](r)
	if r.err != nil {
		return nil, r.err
	}
	// mode is keyed by service name rather than type, so it stays outside
	// the batch.
	mode, err := do.InvokeNamed[config.Mode](i, "mode")
	if err != nil {
		return nil, err
	}

	sm := lifecycle.NewManager()
	sm.Register("usage-flush", NewUsageFlushService(manager, locker))
	sm.Register("cb-watchdog", breaker.NewWatchdog(registry))
	if fr, err := do.Invoke[*debug.FlightRecorderService](i); err == nil {
		sm.Register("flight-recorder", fr)
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
	registerWorkerServices(sm, manager, expirer, ws, locker, cfg)

	if cfg.Reconcile.Enabled {
		reconciler, err := do.Invoke[*worker.Reconciler](i)
		if err != nil {
			return nil, err
		}
		sm.Register("reconcile", worker.NewReconcileService(reconciler, locker, cfg.Reconcile.Interval))
	}
	if notifier, err := do.Invoke[*notify.Notifier](i); err == nil {
		sm.Register("notifications", notifier)
	}

	return sm, nil
}
