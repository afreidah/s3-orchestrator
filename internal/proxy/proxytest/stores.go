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
	dekCacheTTL           = time.Hour
	cleanupConcurrency    = 10
	claimGracePeriod      = 5 * time.Minute
	testInstanceID        = "test-instance"
	detachedUploadCeiling = 64
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
	QuotaBaselines  map[string]core.BackendQuotaUsage

	Metrics           metrics.Deps // when set, installs a collector
	ReplicationFactor func() int   // under-replication gauge; read only when Metrics is set
}

// NewQuotaTracker builds a quota tracker with baselines primed, as production
// does at startup; without them every write is refused. Backends with no
// supplied baseline are unlimited, so a test that does not care about quota
// never has a write refused.
func NewQuotaTracker(names []string, baselines map[string]core.BackendQuotaUsage) *counter.QuotaTracker {
	primed := make(map[string]core.BackendQuotaUsage, len(names))
	for _, name := range names {
		primed[name] = core.BackendQuotaUsage{BackendName: name}
	}
	for name, usage := range baselines {
		primed[name] = usage
	}
	tracker := counter.NewQuotaTracker(names)
	tracker.SetBaselines(primed)
	return tracker
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

	ParallelBroadcast            bool
	DegradedBroadcastParallelism int
	DisableDegradedReads         bool
	BackendTimeout               time.Duration
	CopiesPerWrite               int // above 1, a PUT places its copies itself
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
		Quota:           NewQuotaTracker(names, opts.QuotaBaselines),
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

// NewUsage builds the usage service over a runtime and store.
func NewUsage(rt *infra.BackendRuntime, stores storetest.MetadataStore) *usage.Service {
	return usage.New(&usage.Deps{Usage: rt.Usage(), Quota: rt.Quota(), Stores: stores})
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
	Drainer      *worker.Drainer
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

// CloseStack stops the background goroutines the stack owns. New registers it
// as cleanup; a caller of Build must call it.
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
	coord := writepath.New(rt, store)

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
		Coord:                        coord,
		Stores:                       store,
		Encryptor:                    opts.Encryptor,
		Codec:                        opts.Codec,
		Compression:                  opts.Compression,
		LocationCache:                object.NewLocationCache(opts.CacheTTL),
		ObjectCache:                  opts.ObjectCache,
		ParallelBroadcast:            opts.ParallelBroadcast,
		CopiesPerWrite:               opts.CopiesPerWrite,
		Detached:                     writepath.NewDetachedUploads(detachedUploadCeiling),
		DegradedBroadcastParallelism: opts.DegradedBroadcastParallelism,
		DisableDegradedReads:         opts.DisableDegradedReads,
		IntegrityCfg:                 integrityCfg,
		BackendTimeout:               opts.BackendTimeout,
	})

	dm := drain.New(rt, store, store, store)
	rt.SetDrainChecker(dm)

	return &Stack{
		Runtime:   rt,
		Coord:     coord,
		Objects:   om,
		Multipart: mp,
		Drain:     dm,
		Drainer: worker.NewDrainer(worker.DrainerDeps{
			Ops: rt, Placement: coord, Store: store,
			AbortUploads: mp.AbortMultipartUploadsOnBackend,
			OnRecords:    dm.SetStates,
		}),
		Usage:        NewUsage(rt, store),
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
	Drainer                *worker.Drainer
}

// WorkerFeatures carries the stored-form layers the scrubber and replicator
// must undo to hash the client's bytes. Leaving them zero does not fail loudly:
// the scrubber skips encrypted or compressed copies and still reports a clean
// pass, and the replicator records every new copy unverified.
type WorkerFeatures struct {
	Encryptor *encryption.Encryptor
	Codec     worker.StreamDecompressor
}

// BuildWorkers constructs every worker over the stack with no stored-form
// features. Fixtures with encrypted or compressed objects use
// BuildWorkersWithFeatures.
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
		Drain:   s.Drain,
		Drainer: s.Drainer,
	}
}
