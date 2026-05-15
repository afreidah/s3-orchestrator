// -------------------------------------------------------------------------------
// DI - Explicit Manager Wiring
//
// Author: Alex Freidah
//
// WireManager performs the post-construction assembly that connects the
// BackendManager to its drain manager. Workers are no longer carried as
// fields on BackendManager (every consumer resolves them through DI), so
// WireManager's job has narrowed to: verify every required worker
// provider resolves cleanly (a smoke check at boot rather than at first
// request), then install the drain.Manager via WireDrain.
//
// Called once from cli/serve after NewInjector. The drain.Manager
// installation is the one piece of post-construction wiring on the
// manager  -  documented on the struct, required because of a
// constructor-time dependency cycle between drain.Manager and
// BackendManager.
// -------------------------------------------------------------------------------

package di

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// WireManager resolves the BackendManager plus every required worker
// (as a smoke check that construction succeeded) and installs the drain
// manager onto the BackendManager. Returns the first error from
// resolving a required dependency; the optional PendingReaper Failed
// resolution is logged so a broken provider stays distinguishable from
// an intentionally absent one.
func WireManager(inj do.Injector) error {
	if _, err := do.Invoke[*proxy.BackendManager](inj); err != nil {
		return fmt.Errorf("resolve BackendManager: %w", err)
	}

	if _, err := do.Invoke[*worker.Rebalancer](inj); err != nil {
		return fmt.Errorf("resolve Rebalancer: %w", err)
	}
	if _, err := do.Invoke[*worker.Replicator](inj); err != nil {
		return fmt.Errorf("resolve Replicator: %w", err)
	}
	if _, err := do.Invoke[*worker.OverReplicationCleaner](inj); err != nil {
		return fmt.Errorf("resolve OverReplicationCleaner: %w", err)
	}
	if _, err := do.Invoke[*worker.CleanupWorker](inj); err != nil {
		return fmt.Errorf("resolve CleanupWorker: %w", err)
	}
	if _, err := do.Invoke[*worker.Scrubber](inj); err != nil {
		return fmt.Errorf("resolve Scrubber: %w", err)
	}

	// PendingReaper is optional. A Failed outcome means the provider is
	// registered but its construction errored; log it so a broken-but-
	// configured reaper does not silently turn into "feature off".
	if prRes := Optional[*worker.PendingReaper](inj); prRes.Failed() {
		slog.WarnContext(context.Background(),
			"pending reaper resolution failed; manager will run without it",
			logfmt.Component("di"),
			"error", prRes.Err)
	}

	mgr, err := do.Invoke[*proxy.BackendManager](inj)
	if err != nil {
		return fmt.Errorf("resolve BackendManager: %w", err)
	}
	dm, err := do.Invoke[*drain.Manager](inj)
	if err != nil {
		return fmt.Errorf("resolve DrainManager: %w", err)
	}
	mgr.WireDrain(dm)

	return nil
}
