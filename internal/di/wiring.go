// -------------------------------------------------------------------------------
// DI - Explicit Manager Wiring
//
// Author: Alex Freidah
//
// WireManager verifies that every required worker provider resolves
// cleanly (a smoke check at boot rather than at first request) and points
// the backend runtime's eligibility filter at the drain manager so
// IsDraining reflects live drain state.
//
// Called once from cli/serve after NewInjector.
// -------------------------------------------------------------------------------

package di

import (
	"fmt"
	"log/slog"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// WireManager resolves every required worker (as a smoke check that
// construction succeeded) and points the runtime's eligibility filter at
// the drain manager. Returns the first error from
// resolving a required dependency; the optional PendingReaper Failed
// resolution is logged so a broken provider stays distinguishable from
// an intentionally absent one.
func WireManager(inj do.Injector) error {
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
		//nolint:sloglint // bootstrap warn; no request/span ctx exists during wiring
		slog.Warn("pending reaper resolution failed; manager will run without it",
			logfmt.Component("di"),
			"error", prRes.Err)
	}

	rt, err := do.Invoke[*infra.BackendRuntime](inj)
	if err != nil {
		return fmt.Errorf("resolve BackendRuntime: %w", err)
	}
	dm, err := do.Invoke[*drain.Manager](inj)
	if err != nil {
		return fmt.Errorf("resolve DrainManager: %w", err)
	}
	rt.SetDrainChecker(dm)

	return nil
}
