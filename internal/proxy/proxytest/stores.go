// Package proxytest provides cross-package test helpers for the proxy
// package. Importing it from production code is not supported.
package proxytest

import (
	"log/slog"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/proxy/multipart"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// NewManager builds a *proxy.BackendManager from cfg with the write
// coordinator, multipart manager, and drain manager built and injected so
// cross-package tests get a fully-wired manager (including a live drain
// manager reachable via mgr.Drain()) from a single call.
func NewManager(t testing.TB, cfg *proxy.BackendManagerConfig) *proxy.BackendManager {
	t.Helper()
	return BuildManager(cfg)
}

// BuildManager is NewManager without a testing.TB, for callers that lack
// one such as an integration TestMain. When cfg omits a prebuilt Runtime,
// it assembles one from the flat fields, mirroring di.ProvideBackendRuntime.
func BuildManager(cfg *proxy.BackendManagerConfig) *proxy.BackendManager {
	if cfg != nil && cfg.Runtime == nil {
		cfg.Runtime = backendRuntimeFromConfig(cfg)
	}
	rt := cfg.Runtime
	stores := cfg.Stores.Metadata

	integrityCfg := &syncutil.AtomicConfig[config.IntegrityConfig]{}
	coord := writepath.New(rt, stores, cfg.Policies.PendingEnabled)
	mp := multipart.New(rt, coord, stores, cfg.Features.Encryptor, cfg.Features.ObjectCache, time.Hour, integrityCfg)
	cleanup := worker.NewCleanupWorker(rt, stores, 10, "test-instance", 5*time.Minute)
	dm := drain.New(rt, coord, stores, stores, stores, mp.AbortMultipartUploadsOnBackend, cleanup.ProcessCleanupQueue)

	cfg.Collaborators = proxy.Collaborators{
		Coord:        coord,
		Multipart:    mp,
		Drain:        dm,
		IntegrityCfg: integrityCfg,
	}
	mgr := proxy.NewBackendManager(cfg)
	rt.SetDrainChecker(dm)
	return mgr
}

// backendRuntimeFromConfig builds a *infra.BackendRuntime from the legacy
// flat fields of a test BackendManagerConfig, mirroring
// di.ProvideBackendRuntime.
func backendRuntimeFromConfig(cfg *proxy.BackendManagerConfig) *infra.BackendRuntime {
	backendNames := make([]string, 0, len(cfg.Storage.Backends))
	for name := range cfg.Storage.Backends {
		backendNames = append(backendNames, name)
	}
	counters := cfg.Features.CounterBackend
	if counters == nil {
		counters = counter.NewLocalCounterBackend(backendNames)
	}
	usage := counter.NewUsageTracker(counters, cfg.Policies.UsageLimits)
	rt := infra.New(&infra.Config{
		Backends:        cfg.Storage.Backends,
		Order:           cfg.Storage.Order,
		BackendTimeout:  cfg.Policies.BackendTimeout,
		Usage:           usage,
		RoutingStrategy: cfg.Policies.RoutingStrategy,
		MaxObjectSizes:  cfg.Policies.MaxObjectSizes,
		AdmissionSem:    cfg.Operations.AdmissionSem,
		Log:             slog.Default().With(logfmt.Component("backend_manager")),
	})
	if cfg.Operations.Metrics != nil {
		rt.SetMetricsCollector(metrics.New(cfg.Operations.Metrics, usage, backendNames, cfg.Operations.ReplicationFactor))
	}
	return rt
}

// Workers bundles every worker plus the drain manager a test might need
// to poke. Drain is the manager's own drain.Manager (built by NewManager),
// so eligibility filters and write-path drain checks see the same live
// state the workers do.
type Workers struct {
	Rebalancer             *worker.Rebalancer
	Replicator             *worker.Replicator
	OverReplicationCleaner *worker.OverReplicationCleaner
	CleanupWorker          *worker.CleanupWorker
	PendingReaper          *worker.PendingReaper
	Scrubber               *worker.Scrubber
	Drain                  *drain.Manager
}

// BuildWorkers constructs every worker backed by the supplied metadata
// store. Production code resolves workers through DI; this helper exists
// so mock-based cross-package tests can construct an equivalent set
// without re-implementing each worker's narrow ops surface. Drain is the
// manager's own drain.Manager.
func BuildWorkers(mgr *proxy.BackendManager, m core.MetadataStore) *Workers {
	w := &Workers{}
	w.Rebalancer = worker.NewRebalancer(mgr.Runtime(), mgr, m)
	w.Replicator = worker.NewReplicator(mgr.Runtime(), mgr, m)
	w.OverReplicationCleaner = worker.NewOverReplicationCleaner(mgr.Runtime(), mgr, m)
	w.CleanupWorker = worker.NewCleanupWorker(mgr.Runtime(), m, 10, "test-instance", 5*time.Minute)
	w.PendingReaper = worker.NewPendingReaper(mgr.Runtime(), mgr, m, 0, 0, 0)
	w.Scrubber = worker.NewScrubber(mgr.Runtime(), mgr, m, nil)
	w.Drain = mgr.Drain()
	return w
}
