// -------------------------------------------------------------------------------
// DI - Explicit Manager Wiring
//
// Author: Alex Freidah
//
// WireManager performs the post-construction assembly that connects the
// BackendManager to the workers and drain manager that depend on it. The
// per-worker DI providers stay pure (build and return) so resolving a
// dependency cannot mutate another dependency; the side-effect-shaped
// wiring lives here in one named function so the initialization order is
// explicit and obvious from a single read.
//
// Called once from cli/serve after NewInjector. Optional features that
// are not registered in the current run mode resolve to the zero value
// via invokeOptional; the assignments to BackendManager remain nil for
// those features, which production code already treats as "feature off".
// -------------------------------------------------------------------------------

package di

import (
	"fmt"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// WireManager resolves the BackendManager, every background worker, and
// the drain manager, then assigns the worker handles onto BackendManager
// and installs drain.Manager via WireDrain. Returns the first error from
// resolving a required dependency; optional features resolve to nil
// without erroring, matching the rest of the injector's optional-feature
// behavior.
func WireManager(inj do.Injector) error {
	mgr, err := do.Invoke[*proxy.BackendManager](inj)
	if err != nil {
		return fmt.Errorf("resolve BackendManager: %w", err)
	}

	if mgr.Rebalancer, err = do.Invoke[*worker.Rebalancer](inj); err != nil {
		return fmt.Errorf("resolve Rebalancer: %w", err)
	}
	if mgr.Replicator, err = do.Invoke[*worker.Replicator](inj); err != nil {
		return fmt.Errorf("resolve Replicator: %w", err)
	}
	if mgr.OverReplicationCleaner, err = do.Invoke[*worker.OverReplicationCleaner](inj); err != nil {
		return fmt.Errorf("resolve OverReplicationCleaner: %w", err)
	}
	if mgr.CleanupWorker, err = do.Invoke[*worker.CleanupWorker](inj); err != nil {
		return fmt.Errorf("resolve CleanupWorker: %w", err)
	}
	if mgr.Scrubber, err = do.Invoke[*worker.Scrubber](inj); err != nil {
		return fmt.Errorf("resolve Scrubber: %w", err)
	}

	// PendingReaper provider returns (nil, nil) when the pending pattern
	// is disabled. invokeOptional swallows ErrServiceNotFound the same
	// way other optional consumers do.
	mgr.PendingReaper = invokeOptional[*worker.PendingReaper](inj)

	dm, err := do.Invoke[*drain.Manager](inj)
	if err != nil {
		return fmt.Errorf("resolve DrainManager: %w", err)
	}
	mgr.WireDrain(dm)

	return nil
}
