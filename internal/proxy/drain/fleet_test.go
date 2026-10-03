// -------------------------------------------------------------------------------
// Drain Test Fleet
//
// Author: Alex Freidah
//
// Builds a drain Manager over a live fleet: a real backend runtime over the
// supplied backends and whatever store the test hands it. The purge tests use a
// mock store to assert the calls they care about; the drain-record tests use a
// real SQLite store so start, cancel and progress run against actual records.
// -------------------------------------------------------------------------------

package drain

import (
	"context"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/sqlite"
)

// fleetTimeout bounds a backend call in the test fleet. Long enough that no
// test trips it incidentally.
const fleetTimeout = 30 * time.Second

// drainStore is what New needs from a store, satisfied by both the mock union
// and the SQLite store.
type drainStore interface {
	core.ObjectStore
	core.DrainStore
	core.BackendLifecycleStore
}

// newDrainFleet builds a drain Manager over the supplied backends and store.
func newDrainFleet(t *testing.T, store drainStore, backends map[string]backend.ObjectBackend) (*Manager, *infra.BackendRuntime) {
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
		Quota:           counter.NewQuotaTracker(names),
		RoutingStrategy: config.RoutingPack,
	})
	mgr := New(rt, store, store, store)
	rt.SetDrainChecker(mgr)
	return mgr, rt
}

// newSQLiteStore returns an in-memory SQLite store with a quota row for each
// named backend, so drain records and admission behave as they do in a
// deployment.
func newSQLiteStore(t *testing.T, backends ...string) *sqlite.Store {
	t.Helper()
	ctx := context.Background()
	s, err := sqlite.NewStore(ctx, &config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"}, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(s.Close)
	cfgs := make([]config.BackendConfig, len(backends))
	for i, name := range backends {
		cfgs[i] = config.BackendConfig{Name: name, QuotaBytes: 1 << 30}
	}
	if err := s.SyncQuotaLimits(ctx, cfgs); err != nil {
		t.Fatalf("SyncQuotaLimits: %v", err)
	}
	return s
}
