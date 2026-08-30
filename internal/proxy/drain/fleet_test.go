// -------------------------------------------------------------------------------
// Drain Test Fleet
//
// Author: Alex Freidah
//
// Builds a drain Manager over a live fleet: a real backend runtime and write
// coordinator, with the multipart-abort and cleanup-queue callbacks as inert
// closures. Those two are funcs precisely so drain does not depend on the
// multipart manager or the cleanup worker, and the tests here take the same
// route rather than reaching through a full proxy stack.
//
// The narrow-mock unit tests in manager_test.go cover dispatch wiring; this
// fixture is for the paths whose behaviour depends on real backends holding
// real bytes.
// -------------------------------------------------------------------------------

package drain

import (
	"context"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// fleetTimeout bounds a backend call in the test fleet. Long enough that no
// test trips it incidentally.
const fleetTimeout = 30 * time.Second

// newDrainFleet builds a drain Manager over the supplied backends with an
// inert cleanup-queue callback. store is the wide metadata store; New narrows
// it to the three role interfaces it declares.
func newDrainFleet(
	t *testing.T, store storetest.MetadataStore, backends map[string]backend.ObjectBackend,
) (*Manager, *infra.BackendRuntime) {
	t.Helper()
	return newDrainFleetWithCleanup(t, store, backends, nil)
}

// newDrainFleetWithCleanup is newDrainFleet with a caller-supplied
// cleanup-queue callback, for the tests that assert drain drains the queue
// before it tears a backend down. nil means the queue is a no-op.
func newDrainFleetWithCleanup(
	t *testing.T, store storetest.MetadataStore, backends map[string]backend.ObjectBackend,
	processCleanup func(context.Context) (int, int),
) (*Manager, *infra.BackendRuntime) {
	t.Helper()

	names := make([]string, 0, len(backends))
	for name := range backends {
		names = append(names, name)
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend(names), nil)
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

	if processCleanup == nil {
		processCleanup = func(context.Context) (int, int) { return 0, 0 }
	}
	// Both callbacks are funcs so drain depends on neither the multipart
	// manager nor the cleanup worker. Nothing here aborts uploads.
	mgr := New(rt, writepath.New(rt, store, false), store, store, store,
		func(context.Context, string) {}, processCleanup)
	rt.SetDrainChecker(mgr)
	return mgr, rt
}

// newPermissiveStore returns a union store mock answering every read with an
// empty result, so a drain test states only the queries it asserts on.
func newPermissiveStore(t *testing.T) *storetest.MockMetadataStore {
	t.Helper()
	m := storetest.NewMockMetadataStore(gomock.NewController(t))
	storetest.Permissive(m)
	return m
}
