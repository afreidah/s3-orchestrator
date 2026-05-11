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
	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/notify"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
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
// registers a service for. drain.Manager is invoked too (not registered)
// so its DI side-effect  -  wiring itself into BackendManager  -  runs.
func resolveLifecycleWorkers(i do.Injector) (lifecycleWorkerSet, error) {
	var ws lifecycleWorkerSet
	var err error
	if ws.cleanup, err = do.Invoke[*worker.CleanupWorker](i); err != nil {
		return ws, err
	}
	if ws.rebalancer, err = do.Invoke[*worker.Rebalancer](i); err != nil {
		return ws, err
	}
	if ws.replicator, err = do.Invoke[*worker.Replicator](i); err != nil {
		return ws, err
	}
	if ws.overRep, err = do.Invoke[*worker.OverReplicationCleaner](i); err != nil {
		return ws, err
	}
	if ws.scrubber, err = do.Invoke[*worker.Scrubber](i); err != nil {
		return ws, err
	}
	if _, err = do.Invoke[*drain.Manager](i); err != nil {
		return ws, err
	}
	// PendingReaper provider returns (nil, nil) when the feature is off.
	ws.pendingReaper, _ = do.Invoke[*worker.PendingReaper](i)
	return ws, nil
}

// registerWorkerServices registers the worker-mode lifecycle services on
// sm. Pulled out of ProvideLifecycleManager so that function stays under
// the cognitive-complexity ceiling.
func registerWorkerServices(sm *lifecycle.Manager, mgr *proxy.BackendManager, ws lifecycleWorkerSet, locker core.AdvisoryLocker, cfg *config.Config) {
	sm.Register("multipart-cleanup", NewMultipartCleanupService(mgr, locker, cfg.CleanupQueue.MultipartStaleTimeout))
	sm.Register("cleanup-queue", NewCleanupQueueService(ws.cleanup, locker))
	if svc := NewPendingReaperService(ws.pendingReaper, locker, cfg.WritePath.PendingPattern.ReaperTick); svc != nil {
		sm.Register("pending-reaper", svc)
	}
	sm.Register("rebalancer", NewRebalancerService(mgr, ws.rebalancer, locker))
	sm.Register("replicator", NewReplicatorService(mgr, ws.replicator, locker))
	sm.Register("over-replication", NewOverReplicationService(mgr, ws.overRep, locker))
	sm.Register("lifecycle", NewLifecycleService(mgr, locker))
	sm.Register("scrubber", NewScrubberService(ws.scrubber, locker))
}

// ProvideLifecycleManager creates and registers all background services.
func ProvideLifecycleManager(i do.Injector) (*lifecycle.Manager, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	manager, err := do.Invoke[*proxy.BackendManager](i)
	if err != nil {
		return nil, err
	}
	registry, err := do.Invoke[*breaker.Registry](i)
	if err != nil {
		return nil, err
	}
	locker, err := do.Invoke[core.MetadataStore](i)
	if err != nil {
		return nil, err
	}
	mode, err := do.InvokeNamed[string](i, "mode")
	if err != nil {
		return nil, err
	}

	sm := lifecycle.NewManager()
	sm.Register("usage-flush", NewUsageFlushService(manager, locker))
	sm.Register("cb-watchdog", NewCircuitBreakerWatchdog(registry))

	if mode != "worker" && mode != "all" {
		return sm, nil
	}

	ws, err := resolveLifecycleWorkers(i)
	if err != nil {
		return nil, err
	}
	registerWorkerServices(sm, manager, ws, locker, cfg)

	if cfg.Reconcile.Enabled {
		reconciler, err := do.Invoke[*worker.Reconciler](i)
		if err != nil {
			return nil, err
		}
		sm.Register("reconcile", NewReconcileService(reconciler, locker, cfg.Reconcile.Interval))
	}
	if notifier, err := do.Invoke[*notify.Notifier](i); err == nil {
		sm.Register("notifications", notifier)
	}

	return sm, nil
}
