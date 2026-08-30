// -------------------------------------------------------------------------------
// Write Coordinator Test Fleet
//
// Author: Alex Freidah
//
// Builds a Coordinator over a live fleet: a real backend runtime with real
// backends behind it, which is what the cleanup-enqueue and orphan-byte
// accounting paths need in order to be asserted end to end.
//
// The coordinator is built from writepath.New directly. Nothing here needs a
// composition root - New already names every collaborator the coordinator has.
// -------------------------------------------------------------------------------

package writepath

import (
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// fleetTimeout bounds a backend call in the test fleet. Long enough that no
// test trips it incidentally.
const fleetTimeout = 30 * time.Second

// fleetOpts tunes the test fleet beyond the defaults. The zero value gives
// pack routing, an unlimited local usage counter, and pending writes off.
type fleetOpts struct {
	// Order fixes the fleet order. Defaults to the backend map's keys.
	Order []string
	// PendingEnabled turns on the pending-write pattern.
	PendingEnabled bool
	// UsageLimits applies per-backend API/bandwidth caps.
	UsageLimits map[string]core.UsageLimits
}

// newFleet builds a Coordinator over the supplied backends, returning it with
// the runtime so a test can read the usage counters it charged. store is the
// wide metadata store; New narrows it to CoordinatorStores.
func newFleet(
	t *testing.T, store storetest.MetadataStore, backends map[string]backend.ObjectBackend, opts *fleetOpts,
) (*Coordinator, *infra.BackendRuntime) {
	t.Helper()
	if opts == nil {
		opts = &fleetOpts{}
	}

	names := opts.Order
	if names == nil {
		for name := range backends {
			names = append(names, name)
		}
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend(names), opts.UsageLimits)
	rt := infra.New(&infra.Config{
		Backends:        backends,
		Order:           names,
		BackendTimeout:  fleetTimeout,
		Usage:           usage,
		RoutingStrategy: config.RoutingPack,
	})
	rt.SetMetricsCollector(metrics.New(metrics.CollectorDeps{
		Store: store, Usage: usage, BackendNames: names,
	}))
	return New(rt, store, opts.PendingEnabled), rt
}
