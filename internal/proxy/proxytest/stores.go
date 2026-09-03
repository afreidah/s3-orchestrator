// -------------------------------------------------------------------------------
// Proxytest - Fixture Builders
//
// Author: Alex Freidah
//
// Builders that mirror what internal/di assembles, one collaborator at a time,
// so a test constructs only the pieces it exercises. Stack composes them for a
// test that needs the whole read/write path.
// -------------------------------------------------------------------------------

package proxytest

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	objcache "github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/proxy/multipart"
	"github.com/afreidah/s3-orchestrator/internal/proxy/object"
	"github.com/afreidah/s3-orchestrator/internal/proxy/usage"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// dekCacheTTL and cleanupConcurrency are the fixture's stand-ins for values an
// operator configures. Neither is what any test is asserting on.
const (
	dekCacheTTL        = time.Hour
	cleanupConcurrency = 10
	claimGracePeriod   = 5 * time.Minute
	testInstanceID     = "test-instance"
)

// -------------------------------------------------------------------------
// OPTIONS
// -------------------------------------------------------------------------

// RuntimeOptions carries the fleet topology and the policy knobs a runtime
// reads. The zero value builds an empty fleet with local counters and no
// limits, which suits a test that never reaches a backend.
type RuntimeOptions struct {
	Backends        map[string]backend.ObjectBackend
	Order           []string
	BackendTimeout  time.Duration
	UsageLimits     map[string]core.UsageLimits
	RoutingStrategy config.RoutingStrategy
	MaxObjectSizes  map[string]int64
	AdmissionSem    chan struct{}
	Backend         counter.Backend

	Metrics           metrics.Deps // when set, installs a collector
	ReplicationFactor func() int   // under-replication gauge; read only when Metrics is set
}

// StackOptions carries what the collaborators need beyond the runtime: the
// stored-form features, the caches, and the write-path mode.
type StackOptions struct {
	Runtime     *infra.BackendRuntime
	Encryptor   *encryption.Encryptor
	Codec       object.Codec
	Compression config.CompressionConfig
	ObjectCache objcache.ObjectCache
	CacheTTL    time.Duration

	PendingEnabled bool // selects the PUT-before-COMMIT write path

	ParallelBroadcast            bool
	DegradedBroadcastParallelism int
	DisableDegradedReads         bool
	BackendTimeout               time.Duration
}

// -------------------------------------------------------------------------
// NARROW BUILDERS
// -------------------------------------------------------------------------

// NewRuntime builds a backend runtime the way di.ProvideBackendRuntime does.
// Use it directly when a test exercises fleet, admission or usage behaviour
// and needs nothing that touches the store.
func NewRuntime(opts *RuntimeOptions) *infra.BackendRuntime {
	if opts == nil {
		opts = &RuntimeOptions{}
	}
	names := opts.Order
	if names == nil {
		for name := range opts.Backends {
			names = append(names, name)
		}
	}
	counters := opts.Backend
	if counters == nil {
		counters = counter.NewLocalCounterBackend(names)
	}
	tracker := counter.NewUsageTracker(counters, opts.UsageLimits)
	rt := infra.New(&infra.Config{
		Backends:        opts.Backends,
		Order:           names,
		BackendTimeout:  opts.BackendTimeout,
		Usage:           tracker,
		RoutingStrategy: opts.RoutingStrategy,
		MaxObjectSizes:  opts.MaxObjectSizes,
		AdmissionSem:    opts.AdmissionSem,
		Log:             slog.Default().With(logfmt.Component("proxytest")),
	})
	if opts.Metrics != nil {
		rt.SetMetricsCollector(metrics.New(metrics.CollectorDeps{
			Store:             opts.Metrics,
			Usage:             tracker,
			BackendNames:      names,
			ReplicationFactor: opts.ReplicationFactor,
		}))
	}
	return rt
}

// NewUsage builds the usage service over a runtime and store. drain may be nil,
// which leaves the flush skipping nothing.
func NewUsage(rt *infra.BackendRuntime, stores storetest.MetadataStore, dm *drain.Manager) *usage.Service {
	deps := usage.Deps{Usage: rt.Usage(), Stores: stores}
	if dm != nil {
		deps.Drain = dm
	}
	return usage.New(&deps)
}

// -------------------------------------------------------------------------
// STACK
// -------------------------------------------------------------------------

// Stack is the set of collaborators internal/di builds, assembled the same way
// and handed back as separate values. It carries no behaviour of its own: a
// test reaches for the collaborator it is exercising.
type Stack struct {
	Runtime      *infra.BackendRuntime
	Coord        *writepath.Coordinator
	Objects      *object.Manager
	Multipart    *multipart.Manager
	Drain        *drain.Manager
	Usage        *usage.Service
	IntegrityCfg *syncutil.AtomicConfig[config.IntegrityConfig]
}

// New builds the whole stack over store and registers its teardown, so a test
// cannot leak the cache eviction goroutines by forgetting to. A nil
// opts.Runtime is built from defaults, which suits a test that never reaches a
// backend; most callers pass one from NewRuntime.
func New(t testing.TB, store storetest.MetadataStore, opts *StackOptions) *Stack {
	t.Helper()
	s := Build(store, opts)
	t.Cleanup(func() { CloseStack(s) })
	return s
}

// CloseStack stops the background goroutines the stack owns. New calls it for
// you; a caller that used Build is responsible for it.
//
// A free function rather than a method: Stack is a bag of collaborators with
// no behaviour of its own, and the one thing that could look like behaviour is
// the fixture's own lifecycle rather than the system's.
func CloseStack(s *Stack) {
	s.Objects.LocationCache().Close()
	s.Multipart.Close()
}

// Build is New without a testing.TB, for callers that lack one such as an
// integration TestMain. The caller owns teardown via CloseStack.
func Build(store storetest.MetadataStore, opts *StackOptions) *Stack {
	if opts == nil {
		opts = &StackOptions{}
	}
	rt := opts.Runtime
	if rt == nil {
		rt = NewRuntime(nil)
	}

	// One integrity-config pointer shared by both managers, and one coordinator
	// shared by everything: production wires it this way, and a fixture that
	// hands out two of either lets a test pass against a shape that cannot exist.
	integrityCfg := &syncutil.AtomicConfig[config.IntegrityConfig]{}
	coord := writepath.New(rt, store, opts.PendingEnabled)

	mp := multipart.New(&multipart.Deps{
		Core:         rt,
		Coord:        coord,
		Stores:       store,
		Encryptor:    opts.Encryptor,
		ObjectCache:  opts.ObjectCache,
		DEKCacheTTL:  dekCacheTTL,
		IntegrityCfg: integrityCfg,
	})
	om := object.New(&object.Deps{
		Core:                         rt,
		BroadcastCore:                rt,
		Coord:                        coord,
		Stores:                       store,
		Encryptor:                    opts.Encryptor,
		Codec:                        opts.Codec,
		Compression:                  opts.Compression,
		LocationCache:                object.NewLocationCache(opts.CacheTTL),
		ObjectCache:                  opts.ObjectCache,
		ParallelBroadcast:            opts.ParallelBroadcast,
		DegradedBroadcastParallelism: opts.DegradedBroadcastParallelism,
		DisableDegradedReads:         opts.DisableDegradedReads,
		IntegrityCfg:                 integrityCfg,
		BackendTimeout:               opts.BackendTimeout,
	})

	cleanup := worker.NewCleanupWorker(worker.CleanupWorkerDeps{
		Ops: rt, Store: store, Concurrency: cleanupConcurrency,
		InstanceID: testInstanceID, ClaimGracePeriod: claimGracePeriod,
	})
	processCleanup := func(ctx context.Context) (int, int) {
		sum := cleanup.ProcessCleanupQueue(ctx)
		return sum.Succeeded, sum.Failed
	}
	dm := drain.New(rt, coord, store, store, store, mp.AbortMultipartUploadsOnBackend, processCleanup)
	rt.SetDrainChecker(dm)

	return &Stack{
		Runtime:      rt,
		Coord:        coord,
		Objects:      om,
		Multipart:    mp,
		Drain:        dm,
		Usage:        NewUsage(rt, store, dm),
		IntegrityCfg: integrityCfg,
	}
}

// -------------------------------------------------------------------------
// WORKERS
// -------------------------------------------------------------------------

// Workers bundles every worker plus the drain manager a test might need to
// poke. Drain is the stack's own drain.Manager, so eligibility filters and
// write-path drain checks see the same live state the workers do.
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
// object back. The scrubber and the replicator both need them: each hashes the
// bytes the client wrote, so each has to decrypt and decode a copy first.
//
// A fixture that leaves these zero builds workers that cannot read an encrypted
// or compressed copy at all. It does not fail loudly - the read errors, the
// scrubber counts the copy as skipped and reports a clean pass having verified
// nothing, and the replicator records every new copy unverified.
type WorkerFeatures struct {
	Encryptor *encryption.Encryptor
	Codec     worker.StreamDecompressor
}

// BuildWorkers constructs every worker over the stack's runtime and
// coordinator. Production resolves workers through DI; this exists so
// mock-based cross-package tests can construct an equivalent set without
// re-implementing each worker's narrow ops surface.
//
// The workers are built with no stored-form features, which suits a fixture
// whose objects are stored verbatim. A fixture that writes encrypted or
// compressed objects wants BuildWorkersWithFeatures instead.
func BuildWorkers(s *Stack, m storetest.MetadataStore) *Workers {
	return BuildWorkersWithFeatures(s, m, WorkerFeatures{})
}

// BuildWorkersWithFeatures is BuildWorkers for a fixture whose objects are
// encrypted, compressed, or both, mirroring what di.ProvideScrubber and
// di.ProvideReplicator wire in production.
func BuildWorkersWithFeatures(s *Stack, m storetest.MetadataStore, features WorkerFeatures) *Workers {
	rt, coord := s.Runtime, s.Coord
	return &Workers{
		Rebalancer: worker.NewRebalancer(rt, coord, m),
		Replicator: worker.NewReplicator(worker.ReplicatorDeps{
			Ops:       rt,
			Placement: coord,
			Store:     m,
			Encryptor: features.Encryptor,
			Codec:     features.Codec,
		}),
		OverReplicationCleaner: worker.NewOverReplicationCleaner(rt, coord, m),
		CleanupWorker: worker.NewCleanupWorker(worker.CleanupWorkerDeps{
			Ops: rt, Store: m, Concurrency: cleanupConcurrency,
			InstanceID: testInstanceID, ClaimGracePeriod: claimGracePeriod,
		}),
		PendingReaper: worker.NewPendingReaper(worker.PendingReaperDeps{Ops: rt, Placement: coord, Store: m}),
		Scrubber: worker.NewScrubber(worker.ScrubberDeps{
			Ops:       rt,
			Placement: coord,
			Store:     m,
			Encryptor: features.Encryptor,
			Codec:     features.Codec,
		}),
		Drain: s.Drain,
	}
}
