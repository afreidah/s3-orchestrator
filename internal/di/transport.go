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
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/debug"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/notify"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
	"github.com/afreidah/s3-orchestrator/internal/transport/ui"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// ProvideBucketAuth creates the credential-to-bucket registry.
func ProvideBucketAuth(i do.Injector) (*auth.BucketRegistry, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	return auth.NewBucketRegistry(cfg.Buckets), nil
}

// ProvideS3Server creates the S3-compatible HTTP handler.
func ProvideS3Server(i do.Injector) (*s3api.Server, error) {
	r := newResolver(i)
	cfg := resolve[*config.Config](r)
	manager := resolve[*proxy.BackendManager](r)
	bucketAuth := resolve[*auth.BucketRegistry](r)
	if r.err != nil {
		return nil, r.err
	}
	srv := s3api.NewServer(manager, cfg.Server.MaxObjectSize)
	srv.SetBucketAuth(bucketAuth)
	return srv, nil
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

// ProvideUIHandler creates the web dashboard handler.
func ProvideUIHandler(i do.Injector) (*ui.Handler, error) {
	r := newResolver(i)
	cfg := resolve[*config.Config](r)
	manager := resolve[*proxy.BackendManager](r)
	cb := resolve[*breaker.CircuitBreaker](r)
	logBuffer := resolve[*telemetry.LogBuffer](r)
	loginThrottle := resolve[*httputil.LoginThrottle](r)
	adminHandler := resolve[*admin.Handler](r)
	rebalancer := resolve[*worker.Rebalancer](r)
	overRep := resolve[*worker.OverReplicationCleaner](r)
	if r.err != nil {
		return nil, r.err
	}

	return ui.New(&ui.Deps{
		BackendOps:    manager,
		Objects:       manager.Objects(),
		Rebalancer:    rebalancer,
		OverRep:       overRep,
		AdminHandler:  adminHandler,
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
	cfg        *config.Config
	manager    *proxy.BackendManager
	cb         *breaker.CircuitBreaker
	encAdmin   core.EncryptionAdmin
	logLevel   *slog.LevelVar
	enc        *encryption.Encryptor
	stores     core.MetadataStore
	replicator *worker.Replicator
	rebalancer *worker.Rebalancer
	overRep    *worker.OverReplicationCleaner
	scrubber   *worker.Scrubber
	drain      *drain.Manager
}

// resolveAdminHandlerRequiredDeps invokes every required dependency the
// admin handler needs and bails on the first error.
func resolveAdminHandlerRequiredDeps(i do.Injector) (adminHandlerRequiredDeps, error) {
	r := newResolver(i)
	d := adminHandlerRequiredDeps{
		cfg:        resolve[*config.Config](r),
		manager:    resolve[*proxy.BackendManager](r),
		cb:         resolve[*breaker.CircuitBreaker](r),
		encAdmin:   resolve[core.EncryptionAdmin](r),
		logLevel:   resolve[*slog.LevelVar](r),
		stores:     resolve[core.MetadataStore](r),
		replicator: resolve[*worker.Replicator](r),
		rebalancer: resolve[*worker.Rebalancer](r),
		overRep:    resolve[*worker.OverReplicationCleaner](r),
		scrubber:   resolve[*worker.Scrubber](r),
		drain:      resolve[*drain.Manager](r),
	}
	if r.err != nil {
		return d, r.err
	}
	// enc needs the resolved cfg and has its own error path, so it stays
	// outside the batch.
	enc, err := resolveOptionalEncryptor(i, d.cfg.Encryption.Enabled)
	if err != nil {
		return d, err
	}
	d.enc = enc
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

// ProvideAdminHandler creates the admin API handler.
func ProvideAdminHandler(i do.Injector) (*admin.Handler, error) {
	d, err := resolveAdminHandlerRequiredDeps(i)
	if err != nil {
		return nil, err
	}
	adminToken := d.cfg.UI.AdminToken
	if adminToken == "" {
		adminToken = d.cfg.UI.AdminKey
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
	// FlightRecorder is optional — Optional[*debug.FlightRecorderService]
	// returns a nil Value when the feature is disabled, so the admin
	// handler ends up holding a nil *trace.FlightRecorder and the
	// snapshot endpoint responds 503.
	frRes := Optional[*debug.FlightRecorderService](i)
	deps := &admin.Deps{
		BackendOps:   d.manager,
		RuntimeOps:   d.manager.Runtime(),
		Replicator:   d.replicator,
		Rebalancer:   d.rebalancer,
		OverRep:      d.overRep,
		Drain:        d.drain,
		Scrubber:     d.scrubber,
		Lifecycle:    d.stores,
		DBHealthy:    d.cb.IsHealthy,
		WorkerHealth: workerHealth,
		Encryption:   d.encAdmin,
		Objects:      d.stores,
		Cleanup:      d.stores,
		Encryptor:    d.enc,
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
		deps.Replication = mc
	}
	return admin.New(deps), nil
}

// ProvideNotifier creates the webhook notification system.
func ProvideNotifier(i do.Injector) (*notify.Notifier, error) {
	r := newResolver(i)
	cfg := resolve[*config.Config](r)
	stores := resolve[core.MetadataStore](r)
	if r.err != nil {
		return nil, r.err
	}
	return notify.NewNotifier(&cfg.Notifications, stores), nil
}
