// -------------------------------------------------------------------------------
// DI - Backend, Manager, and Backend-Adjacent Optional Providers
//
// Author: Alex Freidah
//
// Initializes the S3 backend fleet (wrapped in per-backend circuit breakers
// when enabled), the breaker registry the watchdog consumes, and each proxy
// collaborator every transport and worker depends on. Also hosts the optional
// providers whose values feed them: encryption engine + key provider,
// Redis-backed shared counters and the shared state riding them, and the
// object data cache.
// -------------------------------------------------------------------------------

package di

import (
	"context"
	"crypto/tls"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/breaker"
	objcache "github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/instanceid"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/proxy/backendstate"
	"github.com/afreidah/s3-orchestrator/internal/proxy/dashboard"
	"github.com/afreidah/s3-orchestrator/internal/proxy/expiry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/proxy/multipart"
	"github.com/afreidah/s3-orchestrator/internal/proxy/object"
	"github.com/afreidah/s3-orchestrator/internal/proxy/reconcile"
	"github.com/afreidah/s3-orchestrator/internal/proxy/usage"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/sharedstate"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// -------------------------------------------------------------------------
// BACKEND PROVIDERS
// -------------------------------------------------------------------------

// BackendsResult groups the outputs of backend initialization so multiple
// providers can resolve it without re-running construction.
//
// Breakers is carried here so the watchdog registry receives the instances
// directly, rather than rediscovering them by type assertion at runtime.
type BackendsResult struct {
	Backends    map[string]backend.ObjectBackend
	Order       []string
	UsageLimits map[string]core.UsageLimits
	Profiles    map[string]core.BackendProfile
	Breakers    []*backend.CircuitBreakerBackend // empty when per-backend breakers are disabled
}

// ProvideBackends initializes all configured storage backends, wrapping
// each with a per-backend circuit breaker when enabled.
func ProvideBackends(i do.Injector) (*BackendsResult, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}

	limits, err := core.UsageLimitsFor(cfg.Backends)
	if err != nil {
		return nil, err
	}
	backends := make(map[string]backend.ObjectBackend, len(cfg.Backends))
	order := make([]string, 0, len(cfg.Backends))
	profiles := make(map[string]core.BackendProfile, len(cfg.Backends))
	breakers := make([]*backend.CircuitBreakerBackend, 0, len(cfg.Backends))

	for idx := range cfg.Backends {
		bcfg := &cfg.Backends[idx]
		s3be, err := backend.NewS3Backend(context.Background(), bcfg)
		if err != nil {
			return nil, err
		}
		var be backend.ObjectBackend = s3be
		if cfg.BackendCircuitBreaker.Enabled {
			cbBackend := backend.NewCircuitBreakerBackend(s3be, bcfg.Name, cfg.BackendCircuitBreaker)
			breakers = append(breakers, cbBackend)
			be = cbBackend
		}
		backends[bcfg.Name] = be
		order = append(order, bcfg.Name)
		profiles[bcfg.Name] = core.BackendProfile{
			Endpoint:      bcfg.Endpoint,
			Region:        bcfg.Region,
			Bucket:        bcfg.Bucket,
			MaxObjectSize: bcfg.MaxObjectSize,
		}
		//nolint:sloglint // bootstrap log; no request/span ctx exists yet
		slog.Info("backend initialized",
			logfmt.Component("di"),
			"backend", bcfg.Name,
			"endpoint", bcfg.Endpoint,
			"bucket", bcfg.Bucket,
		)
	}

	return &BackendsResult{
		Backends:    backends,
		Order:       order,
		UsageLimits: limits,
		Profiles:    profiles,
		Breakers:    breakers,
	}, nil
}

// ProvideBreakerRegistry assembles the watchdog's breaker registry: a recovery
// prober that pings the database for the database circuit breaker, one for
// each breaker of every backend produced during backend initialization (the
// backend breaker and one per call), and the Redis breaker's when Redis is
// configured. The database ping costs nothing, so its checks do not back off;
// the backend probers charge their checks through the usage tracker.
func ProvideBreakerRegistry(i do.Injector) (*breaker.Registry, error) {
	r := NewResolver(i)
	dbCB := r.Resolve[*breaker.CircuitBreaker]()
	stores := r.Resolve[metadataStore]()
	br := r.Resolve[*BackendsResult]()
	usage := r.Resolve[*counter.UsageTracker]()
	if r.err != nil {
		return nil, r.err
	}
	probers := []breaker.Prober{breaker.NewRecoveryProber(dbCB, breaker.Recovery{Check: stores.Ping})}
	for _, b := range br.Breakers {
		for _, p := range b.Probes() {
			probers = append(probers, breaker.NewRecoveryProber(p.Breaker, p.Recovery(b.Name(), usage)))
		}
	}
	redisRes := Optional[*counter.RedisCounterBackend](i)
	if redisRes.Failed() {
		return nil, redisRes.Err
	}
	if redisRes.Value != nil {
		probers = append(probers, redisRes.Value.Prober())
	}
	return breaker.NewRegistry(probers...), nil
}

// -------------------------------------------------------------------------
// OPTIONAL COMPONENT PROVIDERS
// -------------------------------------------------------------------------

// ProvideEncryptor creates the envelope encryption engine.
func ProvideEncryptor(i do.Injector) (*encryption.Encryptor, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	provider, err := encryption.NewKeyProviderFromConfig(&cfg.Encryption)
	if err != nil {
		return nil, err
	}
	enc, err := encryption.NewEncryptor(provider, cfg.Encryption.ChunkSize)
	if err != nil {
		encryption.CloseProvider(provider)
		return nil, err
	}
	//nolint:sloglint // bootstrap log; no request/span ctx exists yet
	slog.Info("server-side encryption enabled",
		logfmt.Component("di"),
		"chunk_size", cfg.Encryption.ChunkSize,
		"key_id", provider.KeyID(),
	)
	return enc, nil
}

// ProvideCodec creates the compression codec. It is built whether or not
// compression is enabled for writes, because objects already stored compressed
// have to stay readable after an operator turns the feature off.
func ProvideCodec(i do.Injector) (*compression.Codec, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	return compression.NewCodecForLevel(cfg.Compression.Level, cfg.Compression.ChunkSize)
}

// ProvideRedisCounterBackend creates the shared Redis counter backend.
func ProvideRedisCounterBackend(i do.Injector) (*counter.RedisCounterBackend, error) {
	r := NewResolver(i)
	cfg := r.Resolve[*config.Config]()
	br := r.Resolve[*BackendsResult]()
	if r.err != nil {
		return nil, r.err
	}

	redisOpts := &redis.Options{
		Addr:     cfg.Redis.Address,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	}
	if cfg.Redis.TLS {
		redisOpts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	redisClient := redis.NewClient(redisOpts)
	rb := counter.NewRedisCounterBackend(redisClient, cfg.Redis, br.Order)
	//nolint:sloglint // bootstrap log; no request/span ctx exists yet
	slog.Info("Redis shared counters enabled",
		logfmt.Component("di"),
		"address", cfg.Redis.Address,
	)
	return rb, nil
}

// ProvideSharedState builds this instance's handle on the state the instances
// share. With Redis configured it rides the Redis counter backend, whatever
// state Redis is in; without Redis the instance runs alone.
func ProvideSharedState(i do.Injector) (*sharedstate.Shared, error) {
	r := NewResolver(i)
	cfg := r.Resolve[*config.Config]()
	id := r.Resolve[instanceid.ID]()
	if r.err != nil {
		return nil, r.err
	}
	if cfg.Redis == nil {
		return sharedstate.New(nil, id), nil
	}
	rb, err := do.Invoke[*counter.RedisCounterBackend](i)
	if err != nil {
		return nil, err
	}
	return sharedstate.New(rb, id), nil
}

// ProvideObjectCache creates the in-memory LRU object data cache.
func ProvideObjectCache(i do.Injector) (objcache.ObjectCache, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	mc, err := objcache.NewMemoryCache(objcache.MemoryConfig{
		MaxSize:       cfg.Cache.MaxSizeBytes,
		MaxObjectSize: cfg.Cache.MaxObjectSizeBytes,
		TTL:           cfg.Cache.TTL,
	})
	if err != nil {
		return nil, err
	}
	//nolint:sloglint // bootstrap log; no request/span ctx exists yet
	slog.Info("object data cache enabled",
		logfmt.Component("di"),
		"max_size", cfg.Cache.MaxSize,
		"max_object_size", cfg.Cache.MaxObjectSize,
		"ttl", cfg.Cache.TTL,
	)
	return mc, nil
}

// -------------------------------------------------------------------------
// WRITE-PATH COLLABORATOR PROVIDERS
// -------------------------------------------------------------------------

// ProvideWriteCoordinator builds the write coordinator: the shared
// delete/move/orphan-cleanup primitive the manager, multipart manager,
// object manager, and drain manager all route writes through.
func ProvideWriteCoordinator(i do.Injector) (*writepath.Coordinator, error) {
	r := NewResolver(i)
	rt := r.Resolve[*infra.BackendRuntime]()
	stores := r.Resolve[metadataStore]()
	if r.err != nil {
		return nil, r.err
	}
	return writepath.New(rt, stores), nil
}

// ProvideDetachedUploads builds the registry of writes whose copies outlive
// their response. Registered whether or not the fan-out is on, because the
// runtime drains it during shutdown and an empty registry drains instantly.
func ProvideDetachedUploads(i do.Injector) (*writepath.DetachedUploads, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	return writepath.NewDetachedUploads(cfg.WritePath.ParallelCopies.MaxInFlight), nil
}

// writePathDeps is the collaborator set the object and multipart managers
// share: both write through the same coordinator, over the same runtime and
// store, under the same integrity config, with encryption optional.
type writePathDeps struct {
	cfg          *config.Config
	rt           *infra.BackendRuntime
	coord        *writepath.Coordinator
	stores       metadataStore
	integrityCfg syncutil.Section[config.IntegrityConfig]
	codec        *compression.Codec
	enc          *encryption.Encryptor
}

// resolveWritePathDeps resolves the collaborators both write-side managers
// take. The encryptor is resolved after the batch rather than inside it: it
// has its own error path and needs the already-resolved config to decide
// whether to build at all.
func resolveWritePathDeps(i do.Injector) (*writePathDeps, error) {
	r := NewResolver(i)
	d := &writePathDeps{
		cfg:          r.Resolve[*config.Config](),
		rt:           r.Resolve[*infra.BackendRuntime](),
		coord:        r.Resolve[*writepath.Coordinator](),
		stores:       r.Resolve[metadataStore](),
		integrityCfg: r.Resolve[*config.Live]().Integrity(),
		codec:        r.Resolve[*compression.Codec](),
	}
	if r.err != nil {
		return nil, r.err
	}

	enc, err := resolveOptionalEncryptor(i, d.cfg.Encryption.Enabled)
	if err != nil {
		return nil, err
	}
	d.enc = enc
	return d, nil
}

// ProvideMultipartManager builds the multipart upload lifecycle manager.
// Built as its own provider so the drain manager can take its
// AbortMultipartUploadsOnBackend hook directly.
func ProvideMultipartManager(i do.Injector) (*multipart.Manager, error) {
	d, err := resolveWritePathDeps(i)
	if err != nil {
		return nil, err
	}
	// dekCacheTTL pegs how long cached unwrapped DEKs live so UploadPart
	// need not re-unwrap the upload-level DEK on every part.
	const dekCacheTTL = time.Hour
	return multipart.New(&multipart.Deps{
		Core:               d.rt,
		Coord:              d.coord,
		Stores:             d.stores,
		Encryptor:          d.enc,
		Codec:              d.codec,
		Compression:        d.cfg.Compression,
		DEKCacheTTL:        dekCacheTTL,
		IntegrityCfg:       d.integrityCfg,
		EnforceMinPartSize: d.cfg.WritePath.Multipart.IsMinPartSizeEnforced(),
	}), nil
}

// -------------------------------------------------------------------------
// MANAGER PROVIDER
// -------------------------------------------------------------------------

// ProvideUsageTracker builds the per-backend usage tracker that admits and
// charges every backend call. With Redis configured, the counters are shared
// through it, whatever state Redis is in: an unreachable Redis puts the
// counter backend in fallback, not out of the wiring.
func ProvideUsageTracker(i do.Injector) (*counter.UsageTracker, error) {
	r := NewResolver(i)
	cfg := r.Resolve[*config.Config]()
	br := r.Resolve[*BackendsResult]()
	if r.err != nil {
		return nil, r.err
	}
	if cfg.Redis != nil {
		rb, err := do.Invoke[*counter.RedisCounterBackend](i)
		if err != nil {
			return nil, err
		}
		return counter.NewUsageTracker(rb, br.UsageLimits), nil
	}
	return counter.NewUsageTracker(counter.NewLocalCounterBackend(br.Order), br.UsageLimits), nil
}

// ProvideBackendRuntime builds the backend runtime: the fleet registry,
// admission semaphore, timeout policy and error classification, over the
// shared usage tracker. A first-class, independently-resolvable dependency
// that workers, drain and every proxy collaborator share.
func ProvideBackendRuntime(i do.Injector) (*infra.BackendRuntime, error) {
	r := NewResolver(i)
	cfg := r.Resolve[*config.Config]()
	br := r.Resolve[*BackendsResult]()
	usage := r.Resolve[*counter.UsageTracker]()
	if r.err != nil {
		return nil, r.err
	}

	// Baselines are empty until the quota flush service primes them from
	// backend_quotas, which it does before the listener accepts a request.
	return infra.New(&infra.Config{
		Backends:        br.Backends,
		Order:           br.Order,
		BackendTimeout:  cfg.Server.BackendTimeout,
		Usage:           usage,
		Quota:           counter.NewQuotaTracker(),
		RoutingStrategy: cfg.RoutingStrategy,
		Profiles:        br.Profiles,
		AdmissionSem:    admissionSemFor(&cfg.Server),
	}), nil
}

// ProvideMetricsCollector builds the collector that publishes the quota,
// fleet and worker gauges, sharing them through the shared state when several
// instances run.
func ProvideMetricsCollector(i do.Injector) (*metrics.Collector, error) {
	r := NewResolver(i)
	cfg := r.Resolve[*config.Config]()
	live := r.Resolve[*config.Live]()
	store := r.Resolve[metrics.Deps]()
	usage := r.Resolve[*counter.UsageTracker]()
	shared := r.Resolve[*sharedstate.Shared]()
	if r.err != nil {
		return nil, r.err
	}
	return metrics.New(&metrics.CollectorDeps{
		Store:             store,
		Usage:             usage,
		ReplicationFactor: func() int { return live.Load().Replication.Factor },
		Shared:            shared,
		FleetInterval:     cfg.Telemetry.Metrics.FleetInterval,
	}), nil
}

// ProvideObjectManager builds the object CRUD / read-failover manager, which
// the S3 transport receives directly. Also installs a recovery listener on the
// DB breaker so this manager's degraded-mode location cache is cleared the
// moment the DB transitions back to closed.
func ProvideObjectManager(i do.Injector) (*object.Manager, error) {
	d, err := resolveWritePathDeps(i)
	if err != nil {
		return nil, err
	}
	r := NewResolver(i)
	dbBreaker := r.Resolve[*breaker.CircuitBreaker]()
	detached := r.Resolve[*writepath.DetachedUploads]()
	if r.err != nil {
		return nil, r.err
	}
	cb := &d.cfg.CircuitBreaker
	objectManager := object.New(&object.Deps{
		Core:                         d.rt,
		Coord:                        d.coord,
		Stores:                       d.stores,
		Encryptor:                    d.enc,
		Codec:                        d.codec,
		Compression:                  d.cfg.Compression,
		LocationCache:                object.NewLocationCache(cb.CacheTTL),
		ObjectCache:                  Optional[objcache.ObjectCache](i).Get(context.Background()),
		ParallelBroadcast:            cb.ParallelBroadcast,
		CopiesPerWrite:               d.cfg.CopiesPerWrite(),
		Detached:                     detached,
		DegradedBroadcastParallelism: cb.DegradedBroadcastParallelism,
		DisableDegradedReads:         !cb.DegradedReads(),
		IntegrityCfg:                 d.integrityCfg,
		BackendTimeout:               d.cfg.Server.BackendTimeout,
	})

	// Drop stale degraded-mode location cache entries on DB recovery so the
	// next reads use fresh DB lookups instead of remembered broadcast winners.
	// Registered here because the cache being cleared is this manager's.
	dbBreaker.AddOnStateChange(func(info breaker.StateChangeInfo) {
		if info.To == breaker.StateClosed {
			objectManager.LocationCache().Clear()
		}
	})

	return objectManager, nil
}

// ProvideDashboardAggregator builds the web-UI data aggregator, so the admin
// and UI transports read dashboard data from a dependency of their own.
func ProvideDashboardAggregator(i do.Injector) (*dashboard.Aggregator, error) {
	r := NewResolver(i)
	rt := r.Resolve[*infra.BackendRuntime]()
	collector := r.Resolve[*metrics.Collector]()
	stores := r.Resolve[metadataStore]()
	scrubber := r.Resolve[*worker.Scrubber]()
	states := r.Resolve[*backendstate.Manager]()
	if r.err != nil {
		return nil, r.err
	}
	return dashboard.New(&dashboard.Deps{
		Store: stores, Usage: rt.Usage(), Scrub: scrubber,
		Fleet: rt, Snapshots: collector, Drain: states,
	}), nil
}

// ProvideExpiryManager builds the lifecycle-expiration manager over the
// lifecycle section of the running configuration.
func ProvideExpiryManager(i do.Injector) (*expiry.Manager, error) {
	r := NewResolver(i)
	live := r.Resolve[*config.Live]()
	objects := r.Resolve[*object.Manager]()
	stores := r.Resolve[metadataStore]()
	if r.err != nil {
		return nil, r.err
	}
	return expiry.New(stores, objects, live.Lifecycle()), nil
}

// ProvideReconcileManager builds the sync/reconcile orchestrator. Its store
// view is four methods wide, which is why it is not carved out of the backend
// manager's union.
func ProvideReconcileManager(i do.Injector) (*reconcile.Manager, error) {
	r := NewResolver(i)
	rt := r.Resolve[*infra.BackendRuntime]()
	stores := r.Resolve[metadataStore]()
	codec := r.Resolve[*compression.Codec]()
	if r.err != nil {
		return nil, r.err
	}
	return reconcile.NewManager(&reconcile.Deps{
		Backends: rt,
		Stores:   stores,
		Usage:    rt.Acct(),
		Codec:    codec,
	}), nil
}

// ProvideUsageService constructs the usage service: the flush of in-memory
// counters into the store, and the reconcile that corrects the drift the
// incremental counter accumulates.
func ProvideUsageService(i do.Injector) (*usage.Service, error) {
	r := NewResolver(i)
	rt := r.Resolve[*infra.BackendRuntime]()
	stores := r.Resolve[metadataStore]()
	if r.err != nil {
		return nil, r.err
	}
	return usage.New(&usage.Deps{Usage: rt.Usage(), Quota: rt.Quota(), Stores: stores}), nil
}

// admissionSemFor returns the shared admission semaphore, or nil when uncapped.
// In split mode it is the write pool, shared by HTTP writes and the background
// workers, since they do write-like work; otherwise it is the one pool shared
// by everything.
func admissionSemFor(s *config.ServerConfig) chan struct{} {
	if n := s.Admission().WriteCapacity(); n > 0 {
		return make(chan struct{}, n)
	}
	return nil
}
