// Package proxytest provides cross-package test helpers for the proxy
// package. Importing it from production code is not supported.
package proxytest

import (
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// NewManager builds a *proxy.BackendManager from cfg. Kept as a helper
// so cross-package call sites stay grouped under a single import even
// though the underlying constructor is a one-liner.
func NewManager(t testing.TB, cfg *proxy.BackendManagerConfig) *proxy.BackendManager {
	t.Helper()
	return proxy.NewBackendManager(cfg)
}

// Workers bundles every worker plus the drain manager that a test
// might need to poke after BuildWorkers has wired the manager. The
// drain manager is also installed on the supplied *proxy.BackendManager
// via WireDrain so eligibility filters and write-path drain checks see
// the live drain state, matching what di.WireManager does in
// production.
type Workers struct {
	Rebalancer             *worker.Rebalancer
	Replicator             *worker.Replicator
	OverReplicationCleaner *worker.OverReplicationCleaner
	CleanupWorker          *worker.CleanupWorker
	PendingReaper          *worker.PendingReaper
	Scrubber               *worker.Scrubber
	Drain                  *drain.Manager
}

// BuildWorkers constructs every worker (rebalancer, replicator, ...)
// plus the drain manager backed by the supplied metadata store, and
// wires the drain manager onto mgr. Production code resolves workers
// through DI; this helper exists so mock-based cross-package tests can
// construct an equivalent fully-wired set without re-implementing each
// worker's narrow ops surface. Callers that only need the wiring side
// effect (drain.Manager attached to mgr) may ignore the return value.
func BuildWorkers(mgr *proxy.BackendManager, m core.MetadataStore) *Workers {
	w := &Workers{}
	w.Rebalancer = worker.NewRebalancer(mgr, m)
	w.Replicator = worker.NewReplicator(mgr, m)
	w.OverReplicationCleaner = worker.NewOverReplicationCleaner(mgr, m)
	w.CleanupWorker = worker.NewCleanupWorker(mgr, m, 10, "test-instance", 5*time.Minute)
	w.PendingReaper = worker.NewPendingReaper(mgr, m, 0, 0, 0)
	w.Scrubber = worker.NewScrubber(mgr, m, nil)

	w.Drain = drain.New(
		mgr,
		m,
		m,
		m,
		mgr.Multipart().AbortMultipartUploadsOnBackend,
		w.CleanupWorker.ProcessCleanupQueue,
	)
	mgr.WireDrain(w.Drain)
	return w
}
