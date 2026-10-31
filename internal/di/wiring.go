// -------------------------------------------------------------------------------
// DI - Explicit Manager Wiring
//
// Author: Alex Freidah
//
// WireManager verifies that every required worker provider resolves
// cleanly (a smoke check at boot rather than at first request) and points
// the backend runtime's eligibility filters at the backend state manager so
// AcceptsWrites and IsDraining reflect live state. Called once by the runtime
// after NewInjector.
// -------------------------------------------------------------------------------

package di

import (
	"context"
	"log/slog"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/proxy/backendstate"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// WireManager resolves every required worker as a smoke check and points the
// runtime's eligibility filters at the backend state manager. It returns the first
// required resolution error; a Failed PendingReaper is only logged.
func WireManager(inj do.Injector) error {
	r := newResolver(inj)
	r.ResolveNamed[*worker.Rebalancer]("Rebalancer")
	r.ResolveNamed[*worker.Replicator]("Replicator")
	r.ResolveNamed[*worker.OverReplicationCleaner]("OverReplicationCleaner")
	r.ResolveNamed[*worker.CleanupWorker]("CleanupWorker")
	r.ResolveNamed[*worker.Scrubber]("Scrubber")
	rt := r.ResolveNamed[*infra.BackendRuntime]("BackendRuntime")
	states := r.ResolveNamed[*backendstate.Manager]("BackendStateManager")
	if r.err != nil {
		return r.err
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

	rt.SetStateChecker(states)
	// Loaded before the first request so a drained backend is never offered
	// to the write path after a restart. A failure is not fatal: admission
	// reads the records itself, and the drainer reloads them each tick.
	if err := states.Refresh(context.Background()); err != nil {
		//nolint:sloglint // bootstrap warn; no request/span ctx exists during wiring
		slog.Warn("failed to load backend states; refusing writes falls to admission until the next refresh",
			logfmt.Component("di"),
			"error", err)
	}

	return nil
}
