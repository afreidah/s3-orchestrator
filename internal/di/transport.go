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
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin"
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
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	manager, err := do.Invoke[*proxy.BackendManager](i)
	if err != nil {
		return nil, err
	}
	bucketAuth, err := do.Invoke[*auth.BucketRegistry](i)
	if err != nil {
		return nil, err
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
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	manager, err := do.Invoke[*proxy.BackendManager](i)
	if err != nil {
		return nil, err
	}
	cb, err := do.Invoke[*breaker.CircuitBreaker](i)
	if err != nil {
		return nil, err
	}
	logBuffer, err := do.Invoke[*telemetry.LogBuffer](i)
	if err != nil {
		return nil, err
	}
	loginThrottle, err := do.Invoke[*httputil.LoginThrottle](i)
	if err != nil {
		return nil, err
	}
	adminHandler, err := do.Invoke[*admin.Handler](i)
	if err != nil {
		return nil, err
	}
	rebalancer, err := do.Invoke[*worker.Rebalancer](i)
	if err != nil {
		return nil, err
	}
	overRep, err := do.Invoke[*worker.OverReplicationCleaner](i)
	if err != nil {
		return nil, err
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
	overRep    *worker.OverReplicationCleaner
	scrubber   *worker.Scrubber
	drain      *drain.Manager
}

// resolveAdminHandlerRequiredDeps invokes every required dependency the
// admin handler needs and bails on the first error.
func resolveAdminHandlerRequiredDeps(i do.Injector) (adminHandlerRequiredDeps, error) {
	var d adminHandlerRequiredDeps
	var err error
	if d.cfg, err = do.Invoke[*config.Config](i); err != nil {
		return d, err
	}
	if d.manager, err = do.Invoke[*proxy.BackendManager](i); err != nil {
		return d, err
	}
	if d.cb, err = do.Invoke[*breaker.CircuitBreaker](i); err != nil {
		return d, err
	}
	if d.encAdmin, err = do.Invoke[core.EncryptionAdmin](i); err != nil {
		return d, err
	}
	if d.logLevel, err = do.Invoke[*slog.LevelVar](i); err != nil {
		return d, err
	}
	if d.enc, err = resolveOptionalEncryptor(i, d.cfg.Encryption.Enabled); err != nil {
		return d, err
	}
	if d.stores, err = do.Invoke[core.MetadataStore](i); err != nil {
		return d, err
	}
	if d.replicator, err = do.Invoke[*worker.Replicator](i); err != nil {
		return d, err
	}
	if d.overRep, err = do.Invoke[*worker.OverReplicationCleaner](i); err != nil {
		return d, err
	}
	if d.scrubber, err = do.Invoke[*worker.Scrubber](i); err != nil {
		return d, err
	}
	if d.drain, err = do.Invoke[*drain.Manager](i); err != nil {
		return d, err
	}
	return d, nil
}

// toAdminWorkerHealth copies a lifecycle.WorkerHealth slice into the
// admin transport's matching type. The two shapes are intentionally
// kept separate (admin owns its wire contract; lifecycle owns its
// internal type) so this conversion is the single place where field
// drift would surface.
func toAdminWorkerHealth(snaps []lifecycle.WorkerHealth) []admin.WorkerHealth {
	out := make([]admin.WorkerHealth, len(snaps))
	for i, s := range snaps {
		out[i] = admin.WorkerHealth{
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
	var workerHealth func() []admin.WorkerHealth
	if lm, err := do.Invoke[*lifecycle.Manager](i); err == nil {
		workerHealth = func() []admin.WorkerHealth { return toAdminWorkerHealth(lm.Health()) }
	}
	// FlightRecorder is optional — Optional[*debug.FlightRecorderService]
	// returns a nil Value when the feature is disabled, so the admin
	// handler ends up holding a nil *trace.FlightRecorder and the
	// snapshot endpoint responds 503.
	frRes := Optional[*debug.FlightRecorderService](i)
	return admin.New(&admin.Deps{
		BackendOps:   d.manager,
		Replicator:   d.replicator,
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
	}), nil
}

// ProvideNotifier creates the webhook notification system.
func ProvideNotifier(i do.Injector) (*notify.Notifier, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	stores, err := do.Invoke[core.MetadataStore](i)
	if err != nil {
		return nil, err
	}
	return notify.NewNotifier(&cfg.Notifications, stores), nil
}
