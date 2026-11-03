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

// ProvideLiveRegistry provides the holder of the credential registry in force,
// seeded with the one the process starts with. Every surface that
// authenticates reads it; Install swaps it.
func ProvideLiveRegistry(i do.Injector) (*auth.LiveRegistry, error) {
	p, err := do.Invoke[*Provisioned](i)
	if err != nil {
		return nil, err
	}
	return auth.NewLiveRegistry(p.registry), nil
}

// ProvideS3Server creates the S3-compatible HTTP handler.
func ProvideS3Server(i do.Injector) (*s3api.Server, error) {
	r := NewResolver(i)
	cfg := r.Resolve[*config.Config]()
	objects := r.Resolve[*object.Manager]()
	multipartManager := r.Resolve[*multipart.Manager]()
	registry := r.Resolve[*auth.LiveRegistry]()
	if r.err != nil {
		return nil, r.err
	}
	return s3api.NewServer(objects, multipartManager, registry, cfg.Server.MaxObjectSize), nil
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
func ProvideRateLimiter(i do.Injector) (*httputil.RateLimiter, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	rl := httputil.NewRateLimiter(cfg.RateLimit)
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
	r := NewResolver(i)
	live := r.Resolve[*config.Live]()
	objectManager := r.Resolve[*object.Manager]()
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

	enc, err := resolveOptionalEncryptor(i, live.Load().Encryption.Enabled)
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
		Mover:        coord,
		Replicator:   replicator,
		OverRep:      overRep,
		Backlog:      stores,
		Rebalancer:   rebalancer,
		Scrubber:     scrubber,
		Locker:       stores,
		Expiry:       expirer,
		Provisioning: stores,
		Registry:     NewRegistryPublisher(i),
		Declared:     declared,
		Config:       live,
	}), nil
}

// ProvideUIHandler creates the web dashboard handler.
func ProvideUIHandler(i do.Injector) (*ui.Handler, error) {
	r := NewResolver(i)
	live := r.Resolve[*config.Live]()
	cb := r.Resolve[*breaker.CircuitBreaker]()
	logBuffer := r.Resolve[*telemetry.LogBuffer]()
	loginThrottle := r.Resolve[*httputil.LoginThrottle]()
	aggregator := r.Resolve[*dashboard.Aggregator]()
	reconciler := r.Resolve[*reconcile.Manager]()
	opsSvc := r.Resolve[*ops.Services]()
	declared := r.Resolve[*provisioning.Declared]()
	registry := r.Resolve[*auth.LiveRegistry]()
	if r.err != nil {
		return nil, r.err
	}

	return ui.New(&ui.Deps{
		Buckets:       declared,
		Dashboard:     aggregator,
		Sync:          reconciler,
		Ops:           opsSvc,
		DBHealthy:     cb.IsHealthy,
		Config:        live,
		LogBuffer:     logBuffer,
		LoginThrottle: loginThrottle,
		Registry:      registry.Load,
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
// dependencies are resolved together; the optional ones (reconciler, flight
// recorder, cache) leave their endpoints inert when absent.
func ProvideAdminHandler(i do.Injector) (*admin.Handler, error) {
	r := NewResolver(i)
	usageSvc := r.Resolve[*usage.Service]()
	cb := r.Resolve[*breaker.CircuitBreaker]()
	logLevel := r.Resolve[*slog.LevelVar]()
	stores := r.Resolve[metadataStore]()
	states := r.Resolve[*backendstate.Manager]()
	opsSvc := r.Resolve[*ops.Services]()
	aggregator := r.Resolve[*dashboard.Aggregator]()
	runtime := r.Resolve[*infra.BackendRuntime]()
	live := r.Resolve[*config.Live]()
	id := r.Resolve[instanceid.ID]()
	shared := r.Resolve[*sharedstate.Shared]()
	mode := r.Resolve[config.Mode]()
	lm := r.Resolve[*lifecycle.Manager]()
	logBuffer := r.Resolve[*telemetry.LogBuffer]()
	registry := r.Resolve[*auth.LiveRegistry]()
	if r.err != nil {
		return nil, r.err
	}
	ctx := context.Background()
	// A disabled flight recorder leaves the admin handler a nil recorder, and
	// the snapshot endpoint answers 503.
	deps := &admin.Deps{
		BackendOps:   usageSvc,
		Dashboard:    aggregator,
		Ops:          opsSvc,
		States:       states,
		StateStore:   stores,
		DBHealthy:    cb.IsHealthy,
		WorkerHealth: func() []adminapi.WorkerHealth { return toAdminWorkerHealth(lm.Health()) },
		Cleanup:      stores,
		ObjectCache:  Optional[objcache.ObjectCache](i).Get(ctx),
		FlightRec:    Optional[*debug.FlightRecorderService](i).Get(ctx).Recorder(),
		Reconciler:   Optional[*worker.Reconciler](i).Get(ctx),
		Registry:     registry.Load,
		Config:       live,
		LogLevel:     logLevel,
		LogBuffer:    logBuffer,
		Instance: adminapi.InstanceStatus{
			ID:        id.String(),
			Version:   telemetry.Version,
			Mode:      string(mode),
			StartedAt: time.Now().UTC(),
		},
		Shared: shared,
	}
	// Set only when the runtime built one; a nil *metrics.Collector in the
	// interface field would defeat the replication endpoint's nil check.
	if mc := runtime.MetricsCollector(); mc != nil {
		deps.ReplMetrics = mc
	}
	return admin.New(deps), nil
}

// ProvideNotifier creates the webhook notification system.
func ProvideNotifier(i do.Injector) (*notify.Notifier, error) {
	r := NewResolver(i)
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
