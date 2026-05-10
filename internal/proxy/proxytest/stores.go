// Package proxytest provides cross-package test helpers for the proxy
// package. Importing it from production code is not supported.
package proxytest

import (
	"time"

	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// AttachWorkers builds every worker (rebalancer, replicator, ...) plus
// the drain manager and attaches them to mgr. Mock-based cross-package
// tests that hold a *proxy.BackendManager call this with the same
// metadata-store value they fed to the BackendManagerConfig so the
// worker handles on mgr line up with the same dependency. Production
// wiring goes through the DI providers in internal/di.
func AttachWorkers(mgr *proxy.BackendManager, m core.MetadataStore) {
	mgr.Rebalancer = worker.NewRebalancer(mgr, struct {
		core.ObjectStore
		core.QuotaStore
	}{ObjectStore: m, QuotaStore: m})

	mgr.Replicator = worker.NewReplicator(mgr, struct {
		core.ObjectStore
		core.ReplicationStore
		core.QuotaStore
	}{ObjectStore: m, ReplicationStore: m, QuotaStore: m})

	mgr.OverReplicationCleaner = worker.NewOverReplicationCleaner(mgr, struct {
		core.ReplicationStore
		core.QuotaStore
	}{ReplicationStore: m, QuotaStore: m})

	mgr.CleanupWorker = worker.NewCleanupWorker(mgr, m, 10, "test-instance", 5*time.Minute)
	mgr.PendingReaper = worker.NewPendingReaper(mgr, m, 0, 0, 0)
	mgr.Scrubber = worker.NewScrubber(mgr, m, nil)

	mgr.WireDrain(drain.New(
		mgr,
		m,
		m,
		m,
		mgr.MultipartManager.AbortMultipartUploadsOnBackend,
		mgr.CleanupWorker.ProcessCleanupQueue,
	))
}
