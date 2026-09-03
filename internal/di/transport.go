// -------------------------------------------------------------------------------
// DI - HTTP Transport and Handler Providers
//
// Author: Alex Freidah
//
// Wires the S3-compatible API server, admin handler, web UI handler,
// per-IP rate limiter and login throttle, bucket-auth registry, and the
// notification system. Handlers receive their dependencies through narrow
// consumer-defined interfaces (see internal/transport/admin/deps.go) so
// this package can register the implementations without leaking concrete
// proxy/worker types into the transport packages.
// -------------------------------------------------------------------------------

package di

import (
	"cmp"
	"context"
	"log/slog"
	"time"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/debug"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/notify"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/proxy/dashboard"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/proxy/expiry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/proxy/multipart"
	"github.com/afreidah/s3-orchestrator/internal/proxy/object"
	"github.com/afreidah/s3-orchestrator/internal/proxy/reconcile"
	"github.com/afreidah/s3-orchestrator/internal/proxy/usage"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
	"github.com/afreidah/s3-orchestrator/internal/transport/cors"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
	"github.com/afreidah/s3-orchestrator/internal/transport/ui"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// ProvideBucketAuth creates the credential-to-bucket registry.
func ProvideBucketAuth(i do.Injector) (*auth.BucketRegistry, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	return auth.NewBucketRegistry(cfg.Buckets)
}

// ProvideS3Server creates the S3-compatible HTTP handler.
func ProvideS3Server(i do.Injector) (*s3api.Server, error) {
	r := newResolver(i)
	cfg := r.Resolve[*config.Config]()
	objects := r.Resolve[*object.Manager]()
	multipartManager := r.Resolve[*multipart.Manager]()
	bucketAuth := r.Resolve[*auth.BucketRegistry]()
	if r.err != nil {
		return nil, r.err
	}
	srv := s3api.NewServer(objects, multipartManager, cfg.Server.MaxObjectSize)
	srv.SetBucketAuth(bucketAuth)
	return srv, nil
}

// ProvideCORS creates the browser CORS policy with the rules the config
// declares, already compiled.
//
// Registered whether or not any bucket carries rules: the middleware is a
// pass-through for an empty rule set, and installing it unconditionally is
// what lets a reload add the first rule without a restart.
func ProvideCORS(i do.Injector) (*cors.Policy, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	rules, err := cors.NewRegistry(cfg.Buckets)
	if err != nil {
		return nil, err
	}
	policy := cors.New(s3api.BucketFromPath, s3api.WriteS3Error)
	policy.SetRules(rules)
	return policy, nil
}

// ProvideRateLimiter creates the per-IP rate limiter.
func ProvideRateLimiter(i do.Injector) (*s3api.RateLimiter, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	rl := s3api.NewRateLimiter(cfg.RateLimit)
	slog.InfoContext(context.Background(), "rate limiting enabled",
		logfmt.Component("di"),
		"requests_per_sec", cfg.RateLimit.RequestsPerSec,
		"burst", cfg.RateLimit.Burst,
	)
	return rl, nil
}

// ProvideLoginThrottle creates the per-IP login attempt throttle.
func ProvideLoginThrottle(_ do.Injector) (*httputil.LoginThrottle, error) {
	return httputil.NewLoginThrottle(5, 5*time.Minute), nil
}

// ProvideOps creates the operations layer both transports call.
func ProvideOps(i do.Injector) (*ops.Services, error) {
	r := newResolver(i)
	cfg := r.Resolve[*config.Config]()
	rt := r.Resolve[*infra.BackendRuntime]()
	objectManager := r.Resolve[*object.Manager]()
	integrityCfg := r.Resolve[*syncutil.AtomicConfig[config.IntegrityConfig]]()
	stores := r.Resolve[metadataStore]()
	encAdmin := r.Resolve[core.EncryptionAdmin]()
	replicator := r.Resolve[*worker.Replicator]()
	overRep := r.Resolve[*worker.OverReplicationCleaner]()
	rebalancer := r.Resolve[*worker.Rebalancer]()
	scrubber := r.Resolve[*worker.Scrubber]()
	expirer := r.Resolve[*expiry.Manager]()
	if r.err != nil {
		return nil, r.err
	}

	enc, err := resolveOptionalEncryptor(i, cfg.Encryption.Enabled)
	if err != nil {
		return nil, err
	}
	// The codec is resolved whether or not compression is enabled for writes,
	// so decompress-existing can undo a fleet an operator has already turned the
	// feature off for.
	codec, err := do.Invoke[*compression.Codec](i)
	if err != nil {
		return nil, err
	}

	return ops.New(&ops.Deps{
		Objects:      objectManager,
		Store:        stores,
		Encryptor:    enc,
		EncStore:     encAdmin,
		Codec:        codec,
		CompStore:    stores,
		Runtime:      rt,
		Usage:        rt.Usage(),
		IntegrityCfg: integrityCfg,
		Replicator:   replicator,
		OverRep:      overRep,
		Rebalancer:   rebalancer,
		Scrubber:     scrubber,
		Expiry:       expirer,
		Cfg:          cfg,
	}), nil
}

// ProvideUIHandler creates the web dashboard handler.
func ProvideUIHandler(i do.Injector) (*ui.Handler, error) {
	r := newResolver(i)
	cfg := r.Resolve[*config.Config]()
	cb := r.Resolve[*breaker.CircuitBreaker]()
	logBuffer := r.Resolve[*telemetry.LogBuffer]()
	loginThrottle := r.Resolve[*httputil.LoginThrottle]()
	aggregator := r.Resolve[*dashboard.Aggregator]()
	reconciler := r.Resolve[*reconcile.Manager]()
	opsSvc := r.Resolve[*ops.Services]()
	if r.err != nil {
		return nil, r.err
	}

	return ui.New(&ui.Deps{
		Dashboard:     aggregator,
		Sync:          reconciler,
		Objects:       opsSvc.Objects,
		Integrity:     opsSvc.Integrity,
		Replication:   opsSvc.Replication,
		Rebalance:     opsSvc.Rebalance,
		Expiry:        opsSvc.Lifecycle,
		Encryption:    opsSvc.Encryption,
		Compression:   opsSvc.Compression,
		DBHealthy:     cb.IsHealthy,
		Cfg:           cfg,
		LogBuffer:     logBuffer,
		LoginThrottle: loginThrottle,
	}), nil
}

// adminHandlerRequiredDeps is the typed return shape of
// resolveAdminHandlerRequiredDeps. Optional deps (encryptor, object
// cache, reconciler) are resolved separately via Optional[T].
type adminHandlerRequiredDeps struct {
	cfg      *config.Config
	usageSvc *usage.Service
	cb       *breaker.CircuitBreaker
	logLevel *slog.LevelVar
	stores   metadataStore
	drain    *drain.Manager
	ops      *ops.Services
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// resolveAdminHandlerRequiredDeps invokes every required dependency the
// admin handler needs and bails on the first error.
func resolveAdminHandlerRequiredDeps(i do.Injector) (adminHandlerRequiredDeps, error) {
	r := newResolver(i)
	d := adminHandlerRequiredDeps{
		cfg:      r.Resolve[*config.Config](),
		usageSvc: r.Resolve[*usage.Service](),
		cb:       r.Resolve[*breaker.CircuitBreaker](),
		logLevel: r.Resolve[*slog.LevelVar](),
		stores:   r.Resolve[metadataStore](),
		drain:    r.Resolve[*drain.Manager](),
		ops:      r.Resolve[*ops.Services](),
	}
	if r.err != nil {
		return d, r.err
	}
	return d, nil
}

// toAdminWorkerHealth copies a lifecycle.WorkerHealth slice into the
// matching adminapi wire type. The two shapes are intentionally kept
// separate (adminapi owns the wire contract; lifecycle owns its internal
// type) so this conversion is the single place where field drift would
// surface.
func toAdminWorkerHealth(snaps []lifecycle.WorkerHealth) []adminapi.WorkerHealth {
	out := make([]adminapi.WorkerHealth, len(snaps))
	for i, s := range snaps {
		out[i] = adminapi.WorkerHealth{
			Name:                s.Name,
			LastSuccess:         s.LastSuccess,
			LastFailure:         s.LastFailure,
			LastError:           s.LastError,
			ConsecutiveFailures: s.ConsecutiveFailures,
		}
	}
	return out
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// ProvideAdminHandler creates the admin API handler.
func ProvideAdminHandler(i do.Injector) (*admin.Handler, error) {
	d, err := resolveAdminHandlerRequiredDeps(i)
	if err != nil {
		return nil, err
	}
	adminToken := cmp.Or(d.cfg.UI.AdminToken, d.cfg.UI.AdminKey)
	recRes := Optional[*worker.Reconciler](i)
	if recRes.Failed() {
		slog.WarnContext(context.Background(),
			"reconciler resolution failed; admin reconcile endpoint will be inert",
			logfmt.Component("di"),
			"error", recRes.Err)
	}
	// lifecycle.Manager is invoked lazily so a proxy-only deployment
	// that has no worker pool still resolves the admin handler. The
	// closure surfaces a nil snapshot to the admin transport, which
	// then returns 503 to /admin/api/workers.
	var workerHealth func() []adminapi.WorkerHealth
	if lm, err := do.Invoke[*lifecycle.Manager](i); err == nil {
		workerHealth = func() []adminapi.WorkerHealth { return toAdminWorkerHealth(lm.Health()) }
	}
	// FlightRecorder is optional — Optional[*debug.FlightRecorderService]
	// returns a nil Value when the feature is disabled, so the admin
	// handler ends up holding a nil *trace.FlightRecorder and the
	// snapshot endpoint responds 503.
	frRes := Optional[*debug.FlightRecorderService](i)
	aggregator, err := do.Invoke[*dashboard.Aggregator](i)
	if err != nil {
		return nil, err
	}
	deps := &admin.Deps{
		BackendOps:   d.usageSvc,
		Dashboard:    aggregator,
		Objects:      d.ops.Objects,
		Integrity:    d.ops.Integrity,
		Replication:  d.ops.Replication,
		Rebalance:    d.ops.Rebalance,
		Expiry:       d.ops.Lifecycle,
		Encryption:   d.ops.Encryption,
		Compression:  d.ops.Compression,
		Drain:        d.drain,
		Lifecycle:    d.stores,
		DBHealthy:    d.cb.IsHealthy,
		WorkerHealth: workerHealth,
		Cleanup:      d.stores,
		ObjectCache:  resolveOptionalCache(i),
		FlightRec:    frRes.Value.Recorder(),
		Reconciler:   recRes.Value,
		Token:        adminToken,
		LogLevel:     d.logLevel,
	}
	// Set the log buffer only when a real one resolves; assigning a nil
	// *telemetry.LogBuffer to the interface field would make the /logs
	// endpoint's nil-guard miss (non-nil interface holding a nil pointer).
	if lb, err := do.Invoke[*telemetry.LogBuffer](i); err == nil && lb != nil {
		deps.LogBuffer = lb
	}
	// Same guard for the metrics collector behind /admin/api/replication.
	if mc, err := do.Invoke[*metrics.Collector](i); err == nil && mc != nil {
		deps.ReplMetrics = mc
	}
	return admin.New(deps), nil
}

// ProvideNotifier creates the webhook notification system.
func ProvideNotifier(i do.Injector) (*notify.Notifier, error) {
	r := newResolver(i)
	cfg := r.Resolve[*config.Config]()
	stores := r.Resolve[metadataStore]()
	if r.err != nil {
		return nil, r.err
	}
	return notify.NewNotifier(&cfg.Notifications, stores), nil
}
