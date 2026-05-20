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
	"crypto/tls"
	"log/slog"

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
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
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
		s3be, err := backend.NewS3Backend(bcfg)
		if err != nil {
			return nil, err
		}
		var be backend.ObjectBackend = s3be
		if cfg.BackendCircuitBreaker.Enabled {
			cbBackend := backend.NewCircuitBreakerBackend(s3be, bcfg.Name,
				cfg.BackendCircuitBreaker.FailureThreshold,
				cfg.BackendCircuitBreaker.OpenTimeout)
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
		//nolint:sloglint // bootstrap log; no request/span ctx exists yet (#831)
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
	//nolint:sloglint // bootstrap log; no request/span ctx exists yet (#831)
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
	//nolint:sloglint // bootstrap log; no request/span ctx exists yet (#831)
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
	//nolint:sloglint // bootstrap log; no request/span ctx exists yet (#831)
	slog.Info("object data cache enabled",
		logfmt.Component("di"),
		"max_size", cfg.Cache.MaxSize,
		"max_object_size", cfg.Cache.MaxObjectSize,
		"ttl", cfg.Cache.TTL,
	)
	return mc, nil
}

// -------------------------------------------------------------------------
// MANAGER PROVIDER
// -------------------------------------------------------------------------

// ProvideBackendManager creates the central orchestration manager with the
// narrow per-role store interfaces supplied.
func ProvideBackendManager(i do.Injector) (*proxy.BackendManager, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	br, err := do.Invoke[*BackendsResult](i)
	if err != nil {
		return nil, err
	}
	stores, err := do.Invoke[core.MetadataStore](i)
	if err != nil {
		return nil, err
	}
	metricsDeps, err := do.Invoke[metrics.Deps](i)
	if err != nil {
		return nil, err
	}
	enc, err := resolveOptionalEncryptor(i, cfg.Encryption.Enabled)
	if err != nil {
		return nil, err
	}

	return proxy.NewBackendManager(&proxy.BackendManagerConfig{
		Backends:                     br.Backends,
		Stores:                       stores,
		PendingEnabled:               cfg.WritePath.PendingPattern.IsEnabled(),
		Metrics:                      metricsDeps,
		Dashboard:                    stores,
		Order:                        br.Order,
		CacheTTL:                     cfg.CircuitBreaker.CacheTTL,
		BackendTimeout:               cfg.Server.BackendTimeout,
		UsageLimits:                  br.UsageLimits,
		RoutingStrategy:              cfg.RoutingStrategy,
		ParallelBroadcast:            cfg.CircuitBreaker.ParallelBroadcast,
		DegradedBroadcastParallelism: cfg.CircuitBreaker.DegradedBroadcastParallelism,
		Encryptor:                    enc,
		ObjectCache:                  resolveOptionalCache(i),
		CounterBackend:               resolveOptionalCounterBackend(i),
		MaxObjectSizes:               br.MaxObjectSizes,
		AdmissionSem:                 admissionSemFor(&cfg.Server),
		ReplicationFactor:            replicationFactorFromInjector(i),
	})
}

// admissionSemFor returns the shared admission semaphore that lives on
// BackendManager. Behaviour by configuration shape (#835 documents the
// historical intent so future readers do not have to reverse-engineer it
// from the call sites in transport/httpserver/routes.go):
//
//   - Split mode (both MaxConcurrentReads and MaxConcurrentWrites set):
//     the returned channel is sized to MaxConcurrentWrites and acts as
//     the "writes-and-workers" pool. The HTTP read pool is created
//     locally in routes.go as a separate channel sized to
//     MaxConcurrentReads. Background workers (cleanup, replication,
//     rebalance, pending reaper, over-replication) all acquire from this
//     same writes pool via WithAdmission, so reads are isolated from
//     worker activity while writes share their budget with workers.
//   - Merged mode (only MaxConcurrentRequests set): single channel sized
//     to MaxConcurrentRequests acts as the global pool. HTTP reads,
//     HTTP writes, and background workers all contend for the same
//     slots. Simpler to operate; less isolation.
//   - Neither set: returns nil (no admission cap; admission middleware
//     is also not installed in routes.go).
//
// The asymmetry is intentional. Workers do write-like work (DELETE,
// PUT, COPY) so grouping them with HTTP writes keeps the read budget a
// clean ceiling on read-side traffic in split mode. Operators sizing
// MaxConcurrentWrites should account for worker activity as well as
// HTTP write traffic.
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
