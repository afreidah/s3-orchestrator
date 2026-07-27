// -------------------------------------------------------------------------------
// Runtime - Daemon Lifecycle Orchestrator
//
// Author: Alex Freidah
//
// Owns daemon-startup wiring after config is loaded: observability bootstrap,
// DI injector construction, required service resolution, HTTP server build,
// lifecycle manager goroutine, reload coordinator, and ordered shutdown.
// The CLI Run is a thin wrapper that constructs a Runtime and calls Run.
// -------------------------------------------------------------------------------

// Package runtime is the daemon composition root. It assembles every
// long-lived subsystem - observability, DI, the HTTP listener, background
// workers, the reload coordinator - and owns the shutdown order so the
// CLI entry point does not.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/di"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/reload"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httpserver"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// Options carries the inputs the CLI Run passes through to runtime.
type Options struct {
	ConfigPath string
	Mode       config.Mode
	Stdout     io.Writer
}

// Runtime is the composed daemon. Construct with New, then call Run to
// block until ctx is cancelled. Shutdown ordering is owned here.
type Runtime struct {
	opts Options

	cfg    *config.Config
	cfgPtr syncutil.AtomicConfig[config.Config]

	logLevel slog.LevelVar
	obs      *Observability
	log      *slog.Logger

	inj      do.Injector
	manager  *proxy.BackendManager
	http     *httpserver.Server
	reload   *reload.Coordinator
	lifecyc  *lifecycle.Manager
	bgCancel context.CancelFunc
	bgDone   chan struct{}

	ready atomic.Bool
}

// New assembles the runtime. Order matters: config -> observability ->
// DI -> required services -> HTTP -> reload coordinator -> lifecycle
// manager. Each step's failure returns immediately so the caller can
// surface a precise startup error.
func New(opts Options, cfg *config.Config) (*Runtime, error) {
	if cfg == nil {
		return nil, errors.New("runtime: nil config")
	}
	r := &Runtime{opts: opts, cfg: cfg}

	obs, err := startObservability(cfg, opts.Stdout, &r.logLevel)
	if err != nil {
		return nil, fmt.Errorf("init logging: %w", err)
	}
	r.obs = obs
	// Scoped logger built after observability so it inherits the
	// production handler chain (ErrAttrHandler wrapping the JSON
	// handler) rather than the bare default.
	r.log = slog.Default().With(logfmt.Component("runtime"))

	r.inj = di.NewInjector(di.InjectorDeps{Config: cfg, Mode: opts.Mode, LogLevel: &r.logLevel, LogBuffer: obs.LogBuffer})

	manager, err := resolveRequiredServices(r.inj, cfg)
	if err != nil {
		return nil, fmt.Errorf("resolve services: %w", err)
	}
	r.manager = manager
	r.cfgPtr.Store(cfg)

	httpSrv, err := httpserver.New(httpserver.Deps{
		Cfg:       cfg,
		Mode:      opts.Mode,
		Injector:  r.inj,
		Ready:     &r.ready,
		DBBreaker: r.dbBreaker,
	})
	if err != nil {
		return nil, fmt.Errorf("build HTTP server: %w", err)
	}
	r.http = httpSrv

	r.reload = reload.New(&reload.Deps{
		ConfigPath:   opts.ConfigPath,
		Injector:     r.inj,
		CfgPtr:       &r.cfgPtr,
		LogLevel:     &r.logLevel,
		CertReloader: httpSrv.CertReloader(),
	})

	// Wire the reload-status provider into the admin handler after
	// the coordinator exists. Routing through a post-construction
	// setter avoids the admin -> reload -> ui -> admin import cycle.
	if adminHandler, _ := do.Invoke[*admin.Handler](r.inj); adminHandler != nil {
		adminHandler.SetReloadStatusProvider(func() *adminapi.ReloadStatusResponse {
			return toAdminReloadStatus(r.reload.LastResult())
		})
	}

	lifecyc, err := do.Invoke[*lifecycle.Manager](r.inj)
	if err != nil {
		return nil, fmt.Errorf("resolve lifecycle manager: %w", err)
	}
	r.lifecyc = lifecyc

	return r, nil
}

// Run starts background services, the SIGHUP watcher, and the HTTP
// listener; blocks until ctx is cancelled or the HTTP listener errors;
// then performs ordered shutdown. The returned error is the listener's
// error if it surfaced one before ctx was cancelled, otherwise nil.
func (r *Runtime) Run(ctx context.Context) error {
	r.startBackgroundServices()
	r.reload.Watch()

	r.ready.Store(true)
	r.logStartup()

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- r.http.Run(ctx)
	}()

	select {
	case <-ctx.Done():
		// signal.NotifyContext populates context.Cause with the signal that triggered shutdown.
		r.log.InfoContext(ctx, "shutdown initiated", "cause", context.Cause(ctx))
	case err := <-serverErr:
		if err != nil {
			r.shutdown()
			return err
		}
	}

	r.shutdown()

	// Drain any late listener error so the goroutine exits cleanly.
	select {
	case err := <-serverErr:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
	default:
	}

	r.log.InfoContext(ctx, "server stopped")
	return nil
}

// dbBreaker resolves the database circuit breaker from the injector for
// the /health handler. Returns nil when the breaker has not been
// registered (which should be impossible after resolveRequiredServices,
// but the nil check keeps the contract explicit).
func (r *Runtime) dbBreaker() *breaker.CircuitBreaker {
	v, _ := do.Invoke[*breaker.CircuitBreaker](r.inj)
	return v
}

// startBackgroundServices launches the lifecycle manager goroutine. The
// cancel function and done channel let shutdown wait for the manager to
// drain before continuing the teardown sequence.
func (r *Runtime) startBackgroundServices() {
	bgCtx, bgCancel := context.WithCancel(context.Background())
	r.bgCancel = bgCancel
	r.bgDone = make(chan struct{})
	go func() {
		r.lifecyc.Run(bgCtx)
		close(r.bgDone)
	}()
}

// shutdown is the ordered teardown sequence. Each section logs its own
// outcome; later sections do not abort on earlier failures so a
// flaky-but-non-fatal step (e.g. final usage flush) cannot strand
// resources held by the steps that follow.
func (r *Runtime) shutdown() {
	ctx := context.Background()
	r.log.InfoContext(ctx, "shutting down")

	// Deferred so DI-managed resources (providers implementing
	// do.Shutdownable) are always released, even if an earlier step
	// panics or aborts.
	defer func() {
		if r.inj != nil {
			_ = r.inj.Shutdown()
		}
	}()

	if delay := r.cfgPtr.Load().Server.ShutdownDelay; delay > 0 {
		r.log.InfoContext(ctx, "waiting for load balancer deregistration", "delay", delay)
		time.Sleep(delay)
	}

	r.ready.Store(false)
	r.reload.Shutdown()

	shutdownCtx, cancel := context.WithTimeout(ctx, httpserver.DrainTimeout)
	defer cancel()

	r.http.Shutdown(shutdownCtx)

	if rl, _ := do.Invoke[*s3api.RateLimiter](r.inj); rl != nil {
		rl.Close()
	}
	if lt, _ := do.Invoke[*httputil.LoginThrottle](r.inj); lt != nil {
		lt.Close()
	}

	r.bgCancel()
	<-r.bgDone
	r.lifecyc.Stop(10 * time.Second)

	closeEncryptionProvider(r.inj)
	r.manager.Close()

	if err := r.manager.FlushUsage(shutdownCtx); err != nil {
		r.log.WarnContext(shutdownCtx, "final usage flush failed", "error", err)
	}

	closeRedisBackend(shutdownCtx, r.inj, r.log)

	if adminStore, _ := do.Invoke[core.LifecycleAdmin](r.inj); adminStore != nil {
		adminStore.Close()
	}

	if err := r.obs.ShutdownTracer(shutdownCtx); err != nil {
		r.log.ErrorContext(ctx, "tracer shutdown error", "error", err)
	}
}

// closeEncryptionProvider closes the encryption key provider if one was
// resolved and implements an optional Close method. The provider's
// Close method is optional because not every provider holds OS handles.
func closeEncryptionProvider(inj do.Injector) {
	encProvider, err := do.Invoke[encryption.KeyProvider](inj)
	if err != nil {
		return
	}
	if closer, ok := encProvider.(interface{ Close() }); ok {
		closer.Close()
	}
}

// closeRedisBackend closes the Redis counter backend if one was
// registered. Logged at warn because a failure to close Redis on
// shutdown is operationally annoying but not fatal.
func closeRedisBackend(ctx context.Context, inj do.Injector, log *slog.Logger) {
	redisBackend, err := do.Invoke[*counter.RedisCounterBackend](inj)
	if err != nil {
		return
	}
	if err := redisBackend.Close(); err != nil {
		log.WarnContext(ctx, "redis client close failed", "error", err)
	}
}

// logStartup emits the server startup log entry with key configuration.
func (r *Runtime) logStartup() {
	bucketNames := make([]string, len(r.cfg.Buckets))
	for i, b := range r.cfg.Buckets {
		bucketNames[i] = b.Name
	}
	r.log.InfoContext(context.Background(), "S3 Orchestrator starting",
		"version", telemetry.Version,
		"mode", r.opts.Mode,
		"listen_addr", r.cfg.Server.ListenAddr,
		"log_level", r.cfg.Server.LogLevel,
		"buckets", bucketNames,
		"backends", len(r.cfg.Backends),
		"routing_strategy", r.cfg.RoutingStrategy,
	)
}

// toAdminReloadStatus copies a reload result into the admin wire type so no
// internal/reload type reaches the API surface. Returns nil before the first
// reload, which the handler reports as its not-yet placeholder.
func toAdminReloadStatus(res *reload.ReloadResult) *adminapi.ReloadStatusResponse {
	if res == nil {
		return nil
	}
	out := &adminapi.ReloadStatusResponse{
		Status:          string(res.Status),
		Generation:      &res.Generation,
		RequiresRestart: res.RequiresRestart,
		LoadError:       res.LoadError,
		StartedAt:       &res.StartedAt,
		EndedAt:         &res.EndedAt,
	}
	for _, o := range res.Outcomes {
		out.Outcomes = append(out.Outcomes, adminapi.ReloadHookOutcome{
			Name:   o.Name,
			Status: string(o.Status),
			Error:  o.Error,
		})
	}
	return out
}
