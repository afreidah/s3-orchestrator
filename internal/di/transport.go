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
	"context"
	"log/slog"
	"time"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	objcache "github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/debug"
	"github.com/afreidah/s3-orchestrator/internal/instanceid"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/notify"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/provisioning"
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

// ProvideProvisioned loads the provisioning state the process starts with, from
// the config file and the store together. Reloads and provisioning changes
// load and install their own.
func ProvideProvisioned(i do.Injector) (*Provisioned, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	p, err := LoadProvisioning(ctx, i, cfg)
	if err != nil {
		return nil, err
	}
	logAssemblyNotices(ctx, p.registry.Notices())
	return p, nil
}

// ProvideDeclared publishes the bucket set the process starts with.
func ProvideDeclared(i do.Injector) (*provisioning.Declared, error) {
	p, err := do.Invoke[*Provisioned](i)
	if err != nil {
		return nil, err
	}
	declared := provisioning.NewDeclared()
	declared.Set(p.buckets)
	return declared, nil
}

// ProvideBucketAuth provides the credential registry the process starts with.
func ProvideBucketAuth(i do.Injector) (*auth.BucketRegistry, error) {
	p, err := do.Invoke[*Provisioned](i)
	if err != nil {
		return nil, err
	}
	return p.registry, nil
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

// ProvideCORS creates the browser CORS policy with the rules the process
// starts with. It is registered even with no rules, so a reload can add the
// first rule without a restart.
func ProvideCORS(i do.Injector) (*cors.Policy, error) {
	p, err := do.Invoke[*Provisioned](i)
	if err != nil {
		return nil, err
	}
	policy := cors.New(s3api.BucketFromPath, s3api.WriteS3Error)
	policy.SetRules(p.cors)
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
	coord := r.Resolve[*writepath.Coordinator]()
	replicator := r.Resolve[*worker.Replicator]()
	overRep := r.Resolve[*worker.OverReplicationCleaner]()
	rebalancer := r.Resolve[*worker.Rebalancer]()
	scrubber := r.Resolve[*worker.Scrubber]()
	expirer := r.Resolve[*expiry.Manager]()
	declared := r.Resolve[*provisioning.Declared]()
	// The codec is resolved whether or not compression is enabled for writes,
	// so decompress-existing can undo a fleet an operator has already turned the
	// feature off for.
	codec := r.Resolve[*compression.Codec]()
	if r.err != nil {
		return nil, r.err
	}

	enc, err := resolveOptionalEncryptor(i, cfg.Encryption.Enabled)
	if err != nil {
		return nil, err
	}

	return ops.New(&ops.Deps{
		Objects:      objectManager,
		Store:        stores,
		Encryptor:    enc,
		EncStore:     stores,
		Codec:        codec,
		CompStore:    stores,
		Runtime:      rt,
		Mover:        coord,
		IntegrityCfg: integrityCfg,
		Replicator:   replicator,
		OverRep:      overRep,
		Rebalancer:   rebalancer,
		Scrubber:     scrubber,
		Unhashed:     stores,
		Locker:       stores,
		Expiry:       expirer,
		Provisioning: stores,
		Registry:     NewRegistryPublisher(i),
		Declared:     declared,
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
	declared := r.Resolve[*provisioning.Declared]()
	if r.err != nil {
		return nil, r.err
	}

	return ui.New(&ui.Deps{
		Buckets:       declared,
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
		Registry:      adminBucketRegistry(i),
	}), nil
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

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

// ProvideAdminHandler creates the admin API handler. The required
// dependencies are resolved together; the optional ones (reconciler, worker
// health, flight recorder, cache, log buffer) leave their endpoints inert
// when absent.
func ProvideAdminHandler(i do.Injector) (*admin.Handler, error) {
	r := newResolver(i)
	usageSvc := r.Resolve[*usage.Service]()
	cb := r.Resolve[*breaker.CircuitBreaker]()
	logLevel := r.Resolve[*slog.LevelVar]()
	stores := r.Resolve[metadataStore]()
	states := r.Resolve[*backendstate.Manager]()
	opsSvc := r.Resolve[*ops.Services]()
	aggregator := r.Resolve[*dashboard.Aggregator]()
	runtime := r.Resolve[*infra.BackendRuntime]()
	id := r.ResolveNamed[instanceid.ID]("InstanceID")
	shared := r.Resolve[*sharedstate.Shared]()
	mode := r.Resolve[config.Mode]()
	if r.err != nil {
		return nil, r.err
	}
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
	// FlightRecorder is optional - Optional[*debug.FlightRecorderService]
	// returns a nil Value when the feature is disabled, so the admin
	// handler ends up holding a nil *trace.FlightRecorder and the
	// snapshot endpoint responds 503.
	frRes := Optional[*debug.FlightRecorderService](i)
	deps := &admin.Deps{
		BackendOps:   usageSvc,
		Dashboard:    aggregator,
		Objects:      opsSvc.Objects,
		Integrity:    opsSvc.Integrity,
		Replication:  opsSvc.Replication,
		Rebalance:    opsSvc.Rebalance,
		Expiry:       opsSvc.Lifecycle,
		Encryption:   opsSvc.Encryption,
		Compression:  opsSvc.Compression,
		Provision:    opsSvc.Provision,
		States:       states,
		StateStore:   stores,
		DBHealthy:    cb.IsHealthy,
		WorkerHealth: workerHealth,
		Cleanup:      stores,
		ObjectCache:  Optional[objcache.ObjectCache](i).Value,
		FlightRec:    frRes.Value.Recorder(),
		Reconciler:   recRes.Value,
		Registry:     adminBucketRegistry(i),
		BackendNames: runtime.BackendOrder,
		LogLevel:     logLevel,
		Instance: adminapi.InstanceStatus{
			ID:        id.String(),
			Version:   telemetry.Version,
			Mode:      string(mode),
			StartedAt: time.Now().UTC(),
		},
		Shared: shared,
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

// adminBucketRegistry reads the S3 server's current registry on each call, so
// admin auth sees reloaded credentials. It returns nil in a worker-only
// deployment, where provisioned credentials are refused.
func adminBucketRegistry(i do.Injector) func() *auth.BucketRegistry {
	return func() *auth.BucketRegistry {
		res := Optional[*s3api.Server](i)
		if res.Failed() || res.Value == nil {
			return nil
		}
		return res.Value.GetBucketAuth()
	}
}

// ProvideNotifier creates the webhook notification system.
func ProvideNotifier(i do.Injector) (*notify.Notifier, error) {
	r := newResolver(i)
	cfg := r.Resolve[*config.Config]()
	stores := r.Resolve[metadataStore]()
	rt := r.Resolve[*infra.BackendRuntime]()
	if r.err != nil {
		return nil, r.err
	}
	n := notify.NewNotifier(&cfg.Notifications, stores)
	n.SetGaugePublisher(rt)
	return n, nil
}
