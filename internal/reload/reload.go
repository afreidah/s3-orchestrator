// -------------------------------------------------------------------------------
// Reload Coordinator - SIGHUP-Driven Config Refresh
//
// Author: Alex Freidah
//
// Watches for SIGHUP, reloads the configuration file, and fans the new config
// out to every hot-reloadable subsystem. Reload is best-effort per subsystem:
// the cert reloader, bucket auth registry, rate limiter, quota sync, usage
// limits, log level, worker configs, manager config sections, and UI handler
// each apply independently. Non-reloadable field changes are logged with a
// warning so operators know a restart is required.
// -------------------------------------------------------------------------------

// Package reload owns SIGHUP-driven configuration reload. It is the single
// place that knows which config fields are hot-reloadable, in what order
// hooks fire, and which failures degrade the daemon vs which are logged
// and ignored.
package reload

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
	"github.com/afreidah/s3-orchestrator/internal/transport/ui"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// Deps groups everything the coordinator mutates or reads on reload.
type Deps struct {
	// ConfigPath is the on-disk YAML the SIGHUP handler reloads.
	ConfigPath string
	// Injector is consulted for optional subsystems (rate limiter, UI
	// handler) that may not be registered in every mode.
	Injector do.Injector
	// CfgPtr is the atomic config the rest of the process reads. The
	// coordinator atomically swaps in the new config after all hooks fire.
	CfgPtr *syncutil.AtomicConfig[config.Config]
	// LogLevel is updated when Server.LogLevel changes.
	LogLevel *slog.LevelVar
	// CertReloader rotates the TLS certificate; nil when TLS is off.
	CertReloader *httputil.CertReloader
}

// Coordinator owns the SIGHUP goroutine and the apply sequence. Construct
// it with New, then call Watch to start the signal listener. Shutdown
// stops the goroutine.
type Coordinator struct {
	deps Deps

	hupChan chan os.Signal
	hupDone chan struct{}

	mu      sync.Mutex
	stopped bool
}

// New returns a Coordinator with the given deps. Watch must be called to
// start observing SIGHUP.
func New(deps Deps) *Coordinator {
	return &Coordinator{deps: deps}
}

// Watch installs a SIGHUP handler and spawns the reload goroutine. The
// goroutine runs until Shutdown is called.
func (c *Coordinator) Watch() {
	c.hupChan = make(chan os.Signal, 1)
	c.hupDone = make(chan struct{})
	signal.Notify(c.hupChan, syscall.SIGHUP)

	go func() {
		defer close(c.hupDone)
		for range c.hupChan {
			c.Reload()
		}
	}()
}

// Shutdown stops the SIGHUP goroutine and waits for it to exit. Safe to
// call multiple times.
func (c *Coordinator) Shutdown() {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	c.stopped = true
	c.mu.Unlock()

	signal.Stop(c.hupChan)
	close(c.hupChan)
	<-c.hupDone
}

// Reload performs one full reload pass. Exposed so tests can trigger a
// reload without sending a real signal. Load failures abort the reload
// before any atomic state has been touched.
func (c *Coordinator) Reload() {
	ctx := context.Background()
	slog.InfoContext(ctx, "SIGHUP received, reloading configuration", "path", c.deps.ConfigPath)

	newCfg, err := config.LoadConfig(c.deps.ConfigPath)
	if err != nil {
		slog.ErrorContext(ctx, "config reload aborted, keeping current config", "error", err)
		return
	}

	currentCfg := c.deps.CfgPtr.Load()
	for _, w := range config.NonReloadableFieldsChanged(currentCfg, newCfg) {
		slog.WarnContext(ctx, "config field changed but requires restart to take effect", "field", w)
	}

	c.apply(ctx, newCfg)
	c.deps.CfgPtr.Store(newCfg)
	slog.InfoContext(ctx, "configuration reload complete")
}

// apply runs every reload hook. Each hook is best-effort: failures log
// at error or warn level but do not abort subsequent hooks. The order
// matches the historical serve.applyReload sequence so behaviour is
// preserved.
func (c *Coordinator) apply(ctx context.Context, newCfg *config.Config) {
	c.applyCertReload(ctx)
	c.applyBucketAuth(ctx, newCfg)
	c.applyRateLimit(newCfg)

	reloadCtx, reloadCancel := context.WithTimeout(ctx, 10*time.Second)
	defer reloadCancel()

	c.applyQuotaSync(reloadCtx, newCfg)

	manager := optional[*proxy.BackendManager](c.deps.Injector)
	c.applyUsageLimits(manager, newCfg)
	c.applyLogLevel(newCfg)
	c.applyWorkerConfigs(newCfg)
	c.applyManagerConfig(reloadCtx, manager, newCfg)
	c.applyUIHandler(newCfg)
}

// applyCertReload rotates the TLS certificate if a reloader is wired.
func (c *Coordinator) applyCertReload(ctx context.Context) {
	if c.deps.CertReloader == nil {
		return
	}
	if err := c.deps.CertReloader.Reload(); err != nil {
		slog.ErrorContext(ctx, "failed to reload TLS certificate", "error", err)
	}
}

// applyBucketAuth swaps the bucket credentials registry on the S3 server.
func (c *Coordinator) applyBucketAuth(ctx context.Context, newCfg *config.Config) {
	srv := optional[*s3api.Server](c.deps.Injector)
	if srv == nil {
		return
	}
	srv.SetBucketAuth(auth.NewBucketRegistry(newCfg.Buckets))
	slog.InfoContext(ctx, "reloaded bucket credentials", "buckets", len(newCfg.Buckets))
}

// applyRateLimit pushes the new rate-limit knobs onto the limiter when
// the feature is currently enabled. A disabled limiter is intentionally
// left untouched - flipping enabled across reload requires a restart.
func (c *Coordinator) applyRateLimit(newCfg *config.Config) {
	if !newCfg.RateLimit.Enabled {
		return
	}
	rl := optional[*s3api.RateLimiter](c.deps.Injector)
	if rl == nil {
		return
	}
	rl.UpdateLimits(newCfg.RateLimit.RequestsPerSec, newCfg.RateLimit.Burst)
}

// applyQuotaSync re-runs SyncQuotaLimits against the new backend list.
func (c *Coordinator) applyQuotaSync(ctx context.Context, newCfg *config.Config) {
	admin := optional[core.LifecycleAdmin](c.deps.Injector)
	if admin == nil {
		return
	}
	if err := admin.SyncQuotaLimits(ctx, newCfg.Backends); err != nil {
		slog.ErrorContext(ctx, "failed to sync quota limits on reload", "error", err)
	}
}

// applyUsageLimits rebuilds the in-memory per-backend usage limit map
// from the new config and pushes it onto the manager.
func (c *Coordinator) applyUsageLimits(manager *proxy.BackendManager, newCfg *config.Config) {
	if manager == nil {
		return
	}
	limits := make(map[string]core.UsageLimits, len(newCfg.Backends))
	for i := range newCfg.Backends {
		bcfg := &newCfg.Backends[i]
		limits[bcfg.Name] = core.UsageLimits{
			APIRequestLimit:  bcfg.APIRequestLimit,
			EgressByteLimit:  bcfg.EgressByteLimit,
			IngressByteLimit: bcfg.IngressByteLimit,
		}
	}
	manager.UpdateUsageLimits(limits)
}

// applyLogLevel updates the slog level pointer the runtime threaded in.
func (c *Coordinator) applyLogLevel(newCfg *config.Config) {
	if c.deps.LogLevel == nil {
		return
	}
	c.deps.LogLevel.Set(config.ParseLogLevel(newCfg.Server.LogLevel))
}

// applyManagerConfig updates the per-section AtomicConfig fields on
// the manager and refreshes the Prometheus quota gauges.
func (c *Coordinator) applyManagerConfig(ctx context.Context, manager *proxy.BackendManager, newCfg *config.Config) {
	if manager == nil {
		return
	}
	manager.SetUsageFlushConfig(&newCfg.UsageFlush)
	manager.SetLifecycleConfig(&newCfg.Lifecycle)
	manager.SetIntegrityConfig(&newCfg.Integrity)
	if err := manager.UpdateQuotaMetrics(ctx); err != nil {
		slog.WarnContext(ctx, "failed to update quota metrics after reload", "error", err)
	}
}

// applyUIHandler swaps the UI handler's config pointer when the UI is
// registered. UI-disabled deployments silently skip.
func (c *Coordinator) applyUIHandler(newCfg *config.Config) {
	h := optional[*ui.Handler](c.deps.Injector)
	if h == nil {
		return
	}
	h.UpdateConfig(newCfg)
}

// applyWorkerConfigs pushes new Rebalance/Replication/Integrity configs onto
// each worker. Workers that did not get constructed (e.g. api-only mode)
// silently skip.
func (c *Coordinator) applyWorkerConfigs(newCfg *config.Config) {
	if rb := optional[*worker.Rebalancer](c.deps.Injector); rb != nil {
		rb.SetConfig(&newCfg.Rebalance)
	}
	if rp := optional[*worker.Replicator](c.deps.Injector); rp != nil {
		rp.SetConfig(&newCfg.Replication)
	}
	if or := optional[*worker.OverReplicationCleaner](c.deps.Injector); or != nil {
		or.SetConfig(&newCfg.Replication)
	}
	if sc := optional[*worker.Scrubber](c.deps.Injector); sc != nil {
		sc.SetConfig(&newCfg.Integrity)
	}
}

// optional resolves a DI type, returning the zero value if not registered.
// Identical contract to the serve-package invokeOptional helper.
func optional[T any](inj do.Injector) T {
	v, _ := do.Invoke[T](inj)
	return v
}
