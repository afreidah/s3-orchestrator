// -------------------------------------------------------------------------------
// DI - Backend, Manager, and Backend-Adjacent Optional Providers
//
// Author: Alex Freidah
//
// Initializes the S3 backend fleet (wrapped in per-backend circuit breakers
// when enabled), the breaker registry the watchdog consumes, and the
// central proxy.BackendManager that every transport and worker depends on.
// Also hosts the optional providers whose values feed the manager:
// encryption engine + key provider, Redis-backed shared counters, and the
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
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
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

// -------------------------------------------------------------------------
// BACKEND PROVIDERS
// -------------------------------------------------------------------------

// BackendsResult groups the outputs of backend initialization so multiple
// providers can resolve it without re-running construction.
type BackendsResult struct {
	Backends       map[string]backend.ObjectBackend
	Order          []string
	UsageLimits    map[string]core.UsageLimits
	MaxObjectSizes map[string]int64
	// Breakers is the per-backend circuit breakers produced when
	// BackendCircuitBreaker is enabled. Empty when CBs are disabled.
	// The watchdog registry consumes this so it never has to rediscover
	// breakers via type assertion at runtime.
	Breakers []breaker.StaleProbeResetter
}

// ProvideBackends initializes all configured storage backends, wrapping
// each with a per-backend circuit breaker when enabled.
func ProvideBackends(i do.Injector) (*BackendsResult, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}

	backends := make(map[string]backend.ObjectBackend, len(cfg.Backends))
	order := make([]string, 0, len(cfg.Backends))
	limits := make(map[string]core.UsageLimits, len(cfg.Backends))
	maxSizes := make(map[string]int64, len(cfg.Backends))
	breakers := make([]breaker.StaleProbeResetter, 0, len(cfg.Backends))

	for idx := range cfg.Backends {
		bcfg := &cfg.Backends[idx]
		s3be, err := backend.NewS3Backend(context.Background(), bcfg)
		if err != nil {
			return nil, err
		}
		var be backend.ObjectBackend = s3be
		if cfg.BackendCircuitBreaker.Enabled {
			cbBackend := backend.NewCircuitBreakerBackend(s3be, backend.CircuitBreakerConfig{
				Name:      bcfg.Name,
				Threshold: cfg.BackendCircuitBreaker.FailureThreshold,
				Timeout:   cfg.BackendCircuitBreaker.OpenTimeout,
			})
			breakers = append(breakers, cbBackend)
			be = cbBackend
		}
		backends[bcfg.Name] = be
		order = append(order, bcfg.Name)
		limits[bcfg.Name] = core.UsageLimits{
			APIRequestLimit:  bcfg.APIRequestLimit,
			EgressByteLimit:  bcfg.EgressByteLimit,
			IngressByteLimit: bcfg.IngressByteLimit,
		}
		if bcfg.MaxObjectSize > 0 {
			maxSizes[bcfg.Name] = bcfg.MaxObjectSize
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
		Backends:       backends,
		Order:          order,
		UsageLimits:    limits,
		MaxObjectSizes: maxSizes,
		Breakers:       breakers,
	}, nil
}

// ProvideBreakerRegistry assembles the watchdog's breaker registry from the
// database circuit breaker and the per-backend breakers produced during
// backend initialization. Centralizing membership here keeps the watchdog
// itself free of type-assertions and keeps DI as the single wiring point.
func ProvideBreakerRegistry(i do.Injector) (*breaker.Registry, error) {
	dbCB, err := do.Invoke[*breaker.CircuitBreaker](i)
	if err != nil {
		return nil, err
	}
	br, err := do.Invoke[*BackendsResult](i)
	if err != nil {
		return nil, err
	}
	reg := breaker.NewRegistry(dbCB)
	for _, b := range br.Breakers {
		reg.Register(b)
	}
	return reg, nil
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

// ProvideEncryptionProvider creates the key provider for admin key rotation
// operations. Only registered when encryption is enabled.
func ProvideEncryptionProvider(i do.Injector) (encryption.KeyProvider, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	return encryption.NewKeyProviderFromConfig(&cfg.Encryption)
}

// ProvideRedisCounterBackend creates the shared Redis counter backend.
func ProvideRedisCounterBackend(i do.Injector) (*counter.RedisCounterBackend, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	br, err := do.Invoke[*BackendsResult](i)
	if err != nil {
		return nil, err
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
	rb, err := counter.NewRedisCounterBackend(redisClient, cfg.Redis, br.Order)
	if err != nil {
		return nil, err
	}
	//nolint:sloglint // bootstrap log; no request/span ctx exists yet
	slog.Info("Redis shared counters enabled",
		logfmt.Component("di"),
		"address", cfg.Redis.Address,
	)
	return rb, nil
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

// ProvideIntegrityConfig provides the atomic integrity config shared by the
// multipart manager and the object manager so a SIGHUP flips both write
// paths' hashing behavior in one swap.
func ProvideIntegrityConfig(_ do.Injector) (*syncutil.AtomicConfig[config.IntegrityConfig], error) {
	return &syncutil.AtomicConfig[config.IntegrityConfig]{}, nil
}

// ProvideWriteCoordinator builds the write coordinator: the shared
// delete/move/orphan-cleanup primitive the manager, multipart manager,
// object manager, and drain manager all route writes through.
func ProvideWriteCoordinator(i do.Injector) (*writepath.Coordinator, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	rt, err := do.Invoke[*infra.BackendRuntime](i)
	if err != nil {
		return nil, err
	}
	stores, err := do.Invoke[core.MetadataStore](i)
	if err != nil {
		return nil, err
	}
	return writepath.New(rt, stores, cfg.WritePath.PendingPattern.IsEnabled()), nil
}

// ProvideMultipartManager builds the multipart upload lifecycle manager.
// Built outside the BackendManager so the drain manager can take its
// AbortMultipartUploadsOnBackend hook directly.
func ProvideMultipartManager(i do.Injector) (*multipart.Manager, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	rt, err := do.Invoke[*infra.BackendRuntime](i)
	if err != nil {
		return nil, err
	}
	coord, err := do.Invoke[*writepath.Coordinator](i)
	if err != nil {
		return nil, err
	}
	stores, err := do.Invoke[core.MetadataStore](i)
	if err != nil {
		return nil, err
	}
	integrityCfg, err := do.Invoke[*syncutil.AtomicConfig[config.IntegrityConfig]](i)
	if err != nil {
		return nil, err
	}
	enc, err := resolveOptionalEncryptor(i, cfg.Encryption.Enabled)
	if err != nil {
		return nil, err
	}
	// dekCacheTTL pegs how long cached unwrapped DEKs live so UploadPart
	// need not re-unwrap the upload-level DEK on every part.
	const dekCacheTTL = time.Hour
	return multipart.New(&multipart.Deps{
		Core:         rt,
		Coord:        coord,
		Stores:       stores,
		Encryptor:    enc,
		ObjectCache:  resolveOptionalCache(i),
		DEKCacheTTL:  dekCacheTTL,
		IntegrityCfg: integrityCfg,
	}), nil
}

// -------------------------------------------------------------------------
// MANAGER PROVIDER
// -------------------------------------------------------------------------

// ProvideBackendManager creates the central orchestration manager with the
// narrow per-role store interfaces supplied. Also installs a recovery
// listener on the DB breaker so the degraded-mode location cache is
// cleared the moment the DB transitions back to closed.
// ProvideBackendRuntime builds the backend runtime: the fleet registry,
// usage tracker, admission semaphore, timeout policy, error classification,
// and metrics collector. Constructing it here (rather than inside
// NewBackendManager) makes the runtime a first-class, independently-resolvable
// dependency that workers, drain, and the manager all share, instead of a
// promoted embed only reachable through the manager.
func ProvideBackendRuntime(i do.Injector) (*infra.BackendRuntime, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	br, err := do.Invoke[*BackendsResult](i)
	if err != nil {
		return nil, err
	}
	metricsDeps, err := do.Invoke[metrics.Deps](i)
	if err != nil {
		return nil, err
	}

	backendNames := make([]string, 0, len(br.Backends))
	for name := range br.Backends {
		backendNames = append(backendNames, name)
	}

	counters := resolveOptionalCounterBackend(i)
	if counters == nil {
		counters = counter.NewLocalCounterBackend(backendNames)
	}
	usage := counter.NewUsageTracker(counters, br.UsageLimits)

	rt := infra.New(&infra.Config{
		Backends:        br.Backends,
		Order:           br.Order,
		BackendTimeout:  cfg.Server.BackendTimeout,
		Usage:           usage,
		RoutingStrategy: cfg.RoutingStrategy,
		MaxObjectSizes:  br.MaxObjectSizes,
		AdmissionSem:    admissionSemFor(&cfg.Server),
		Log:             slog.Default().With(logfmt.Component("backend_manager")),
	})
	collector := metrics.New(metrics.CollectorDeps{
		Store:             metricsDeps,
		Usage:             usage,
		BackendNames:      backendNames,
		ReplicationFactor: replicationFactorFromInjector(i),
	})
	rt.SetMetricsCollector(collector)
	// Register the collector so the admin handler can serve its replication
	// snapshot from a cheap /admin/api/replication read.
	do.ProvideValue(i, collector)
	return rt, nil
}

func ProvideBackendManager(i do.Injector) (*proxy.BackendManager, error) {
	r := newResolver(i)
	cfg := resolve[*config.Config](r)
	runtime := resolve[*infra.BackendRuntime](r)
	br := resolve[*BackendsResult](r)
	stores := resolve[core.MetadataStore](r)
	metricsDeps := resolve[metrics.Deps](r)
	dbBreaker := resolve[*breaker.CircuitBreaker](r)
	coord := resolve[*writepath.Coordinator](r)
	multipartManager := resolve[*multipart.Manager](r)
	integrityCfg := resolve[*syncutil.AtomicConfig[config.IntegrityConfig]](r)
	drainManager := resolve[*drain.Manager](r)
	if r.err != nil {
		return nil, r.err
	}

	// Encryptor has its own error path and needs the resolved cfg, so it stays
	// outside the batch.
	enc, err := resolveOptionalEncryptor(i, cfg.Encryption.Enabled)
	if err != nil {
		return nil, err
	}

	mgr := proxy.NewBackendManager(&proxy.BackendManagerConfig{
		Runtime: runtime,
		Storage: proxy.StorageDeps{
			Backends: br.Backends,
			Order:    br.Order,
		},
		Stores: proxy.StoreDeps{
			Metadata:  stores,
			Dashboard: stores,
		},
		Policies: proxy.PolicyConfig{
			PendingEnabled:               cfg.WritePath.PendingPattern.IsEnabled(),
			CacheTTL:                     cfg.CircuitBreaker.CacheTTL,
			BackendTimeout:               cfg.Server.BackendTimeout,
			UsageLimits:                  br.UsageLimits,
			RoutingStrategy:              cfg.RoutingStrategy,
			ParallelBroadcast:            cfg.CircuitBreaker.ParallelBroadcast,
			DegradedBroadcastParallelism: cfg.CircuitBreaker.DegradedBroadcastParallelism,
			DisableDegradedReads:         cfg.CircuitBreaker.DegradedReadsEnabled != nil && !*cfg.CircuitBreaker.DegradedReadsEnabled,
			MaxObjectSizes:               br.MaxObjectSizes,
		},
		Features: proxy.FeatureDeps{
			Encryptor:      enc,
			ObjectCache:    resolveOptionalCache(i),
			CounterBackend: resolveOptionalCounterBackend(i),
		},
		Operations: proxy.OperationalDeps{
			Metrics:           metricsDeps,
			AdmissionSem:      admissionSemFor(&cfg.Server),
			ReplicationFactor: replicationFactorFromInjector(i),
		},
		Collaborators: proxy.Collaborators{
			Coord:        coord,
			Multipart:    multipartManager,
			Drain:        drainManager,
			IntegrityCfg: integrityCfg,
		},
	})

	// Drop stale degraded-mode location cache entries on DB recovery so
	// the next reads use fresh DB lookups instead of remembered winners.
	dbBreaker.AddOnStateChange(func(info breaker.StateChangeInfo) {
		if info.To == breaker.StateClosed {
			mgr.ClearCache()
		}
	})

	return mgr, nil
}

// admissionSemFor returns the shared admission semaphore that lives on
// BackendManager. In split mode the returned channel is sized to
// MaxConcurrentWrites and is shared by HTTP writes and all background
// workers (cleanup, replication, rebalance, pending reaper,
// over-replication); reads run on a separate read-only pool created in
// routes.go. In merged mode a single channel sized to
// MaxConcurrentRequests is shared by everything. Returns nil when
// neither is set (no admission cap). The split-mode grouping is
// deliberate: workers do write-like work, so operators sizing
// MaxConcurrentWrites must account for worker traffic too.
func admissionSemFor(s *config.ServerConfig) chan struct{} {
	switch {
	case s.MaxConcurrentReads > 0 && s.MaxConcurrentWrites > 0:
		return make(chan struct{}, s.MaxConcurrentWrites)
	case s.MaxConcurrentRequests > 0:
		return make(chan struct{}, s.MaxConcurrentRequests)
	default:
		return nil
	}
}

// replicationFactorFromInjector returns a closure that lazily resolves
// the replicator from i and reads its hot-reloadable factor. Returns 0
// when replication is disabled or the replicator hasn't been registered
// yet (api mode).
func replicationFactorFromInjector(i do.Injector) func() int {
	return func() int {
		rep, err := do.Invoke[*worker.Replicator](i)
		if err != nil {
			return 0
		}
		if rc := rep.Config(); rc != nil {
			return rc.Factor
		}
		return 0
	}
}
