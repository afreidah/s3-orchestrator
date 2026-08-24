// Package proxytest provides cross-package test helpers for the proxy
// package. Importing it from production code is not supported.
package proxytest

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/proxy/multipart"
	"github.com/afreidah/s3-orchestrator/internal/proxy/object"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// NewManager builds a *proxy.BackendManager from cfg with the write
// coordinator, multipart manager, and drain manager built and injected so
// cross-package tests get a fully-wired manager (including a live drain
// manager reachable via mgr.Drain()) from a single call.
func NewManager(t testing.TB, store core.MetadataStore, cfg *proxy.BackendManagerConfig) *proxy.BackendManager {
	t.Helper()
	return BuildManager(store, cfg)
}

// BuildManager is NewManager without a testing.TB, for callers that lack
// one such as an integration TestMain. When cfg omits a prebuilt Runtime,
// it assembles one from the flat fields, mirroring di.ProvideBackendRuntime.
// store is the wide metadata store the sub-managers are built over.
// BackendManager itself now takes only the narrow usage surface, so the
// fixture supplies the wide value separately rather than recovering it from
// the config.
func BuildManager(store core.MetadataStore, cfg *proxy.BackendManagerConfig) *proxy.BackendManager {
	if cfg != nil && cfg.Runtime == nil {
		cfg.Runtime = backendRuntimeFromConfig(cfg)
	}
	if cfg.Stores.Metadata == nil {
		cfg.Stores.Metadata = store
	}
	rt := cfg.Runtime
	stores := store

	integrityCfg := &syncutil.AtomicConfig[config.IntegrityConfig]{}
	coord := writepath.New(rt, stores, cfg.Policies.PendingEnabled)
	mp := multipart.New(&multipart.Deps{
		Core:         rt,
		Coord:        coord,
		Stores:       stores,
		Encryptor:    cfg.Features.Encryptor,
		ObjectCache:  cfg.Features.ObjectCache,
		DEKCacheTTL:  time.Hour,
		IntegrityCfg: integrityCfg,
	})
	om := object.New(&object.Deps{
		Core:                         rt,
		BroadcastCore:                rt,
		Coord:                        coord,
		Stores:                       stores,
		Encryptor:                    cfg.Features.Encryptor,
		Codec:                        cfg.Features.Codec,
		Compression:                  cfg.Features.Compression,
		LocationCache:                object.NewLocationCache(cfg.Policies.CacheTTL),
		ObjectCache:                  cfg.Features.ObjectCache,
		ParallelBroadcast:            cfg.Policies.ParallelBroadcast,
		DegradedBroadcastParallelism: cfg.Policies.DegradedBroadcastParallelism,
		DisableDegradedReads:         cfg.Policies.DisableDegradedReads,
		IntegrityCfg:                 integrityCfg,
		BackendTimeout:               cfg.Policies.BackendTimeout,
	})
	cleanup := worker.NewCleanupWorker(worker.CleanupWorkerDeps{Ops: rt, Store: stores, Concurrency: 10, InstanceID: "test-instance", ClaimGracePeriod: 5 * time.Minute})
	processCleanup := func(ctx context.Context) (int, int) {
		sum := cleanup.ProcessCleanupQueue(ctx)
		return sum.Succeeded, sum.Failed
	}
	dm := drain.New(rt, coord, stores, stores, stores, mp.AbortMultipartUploadsOnBackend, processCleanup)

	cfg.Collaborators = proxy.Collaborators{
		Coord:        coord,
		Multipart:    mp,
		Objects:      om,
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
		rt.SetMetricsCollector(metrics.New(metrics.CollectorDeps{
			Store:             cfg.Operations.Metrics,
			Usage:             usage,
			BackendNames:      backendNames,
			ReplicationFactor: cfg.Operations.ReplicationFactor,
		}))
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

// WorkerFeatures carries the stored-form layers a worker has to undo to read an
// object back. Only the scrubber needs them: it hashes the bytes the client
// wrote, so it has to decrypt and decode a copy before hashing it.
//
// A fixture that leaves these zero builds a scrubber that cannot read an
// encrypted or compressed copy at all. It does not fail loudly - the read
// errors, the copy is counted as skipped, and the sweep reports a clean pass
// having verified nothing.
type WorkerFeatures struct {
	Encryptor *encryption.Encryptor
	Codec     worker.StreamDecompressor
}

// BuildWorkers constructs every worker backed by the supplied metadata
// store. Production code resolves workers through DI; this helper exists
// so mock-based cross-package tests can construct an equivalent set
// without re-implementing each worker's narrow ops surface. Drain is the
// manager's own drain.Manager.
//
// The workers are built with no stored-form features, which suits a fixture
// whose objects are stored verbatim. A fixture that writes encrypted or
// compressed objects wants BuildWorkersWithFeatures instead.
func BuildWorkers(mgr *proxy.BackendManager, m core.MetadataStore) *Workers {
	return BuildWorkersWithFeatures(mgr, m, WorkerFeatures{})
}

// BuildWorkersWithFeatures is BuildWorkers for a fixture whose objects are
// encrypted, compressed, or both, mirroring what di.ProvideScrubber wires in
// production.
func BuildWorkersWithFeatures(mgr *proxy.BackendManager, m core.MetadataStore, features WorkerFeatures) *Workers {
	// The manager's own coordinator, so a worker's placement decisions run
	// against the same write-path state the manager writes through.
	coord := mgr.Coordinator()
	w := &Workers{}
	w.Rebalancer = worker.NewRebalancer(mgr.Runtime(), coord, m)
	w.Replicator = worker.NewReplicator(mgr.Runtime(), coord, m)
	w.OverReplicationCleaner = worker.NewOverReplicationCleaner(mgr.Runtime(), coord, m)
	w.CleanupWorker = worker.NewCleanupWorker(worker.CleanupWorkerDeps{Ops: mgr.Runtime(), Store: m, Concurrency: 10, InstanceID: "test-instance", ClaimGracePeriod: 5 * time.Minute})
	w.PendingReaper = worker.NewPendingReaper(worker.PendingReaperDeps{Ops: mgr.Runtime(), Placement: coord, Store: m})
	w.Scrubber = worker.NewScrubber(worker.ScrubberDeps{
		Ops:       mgr.Runtime(),
		Placement: coord,
		Store:     m,
		Encryptor: features.Encryptor,
		Codec:     features.Codec,
	})
	w.Drain = mgr.Drain()
	return w
}
