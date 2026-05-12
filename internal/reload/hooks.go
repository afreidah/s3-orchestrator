// -------------------------------------------------------------------------------
// Reload Coordinator - Concrete Hooks
//
// Author: Alex Freidah
//
// One Hook implementation per reloadable subsystem: TLS cert rotator,
// S3 bucket auth registry, rate limiter, quota sync, per-backend usage
// limits, slog level, worker configs, manager config sections, and the
// UI handler. Each hook resolves its dependencies through the injector
// (or a coordinator-owned handle for the cert reloader) so the coordinator
// stays a plain orchestrator that does not import any individual subsystem.
// -------------------------------------------------------------------------------

package reload

import (
	"context"
	"log/slog"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
	"github.com/afreidah/s3-orchestrator/internal/transport/ui"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// defaultHooks returns the standard set of reload hooks the runtime
// wires in. Order matches the historical applyReload sequence so
// existing behaviour is preserved. The cert reloader is passed
// directly because it is not registered in the injector.
func defaultHooks(inj do.Injector, certReloader *httputil.CertReloader, logLevel *slog.LevelVar) []Hook {
	return []Hook{
		&tlsCertHook{reloader: certReloader},
		&bucketAuthHook{inj: inj},
		&rateLimitHook{inj: inj},
		&quotaSyncHook{inj: inj},
		&usageLimitsHook{inj: inj},
		&logLevelHook{level: logLevel},
		&workerConfigsHook{inj: inj},
		&managerConfigHook{inj: inj},
		&uiHandlerHook{inj: inj},
	}
}

// -------------------------------------------------------------------------
// TLS CERTIFICATE
// -------------------------------------------------------------------------

// tlsCertHook rotates the listener's TLS certificate by re-reading
// the cert/key pair from disk. Skipped when TLS is not configured.
type tlsCertHook struct {
	reloader *httputil.CertReloader
}

func (*tlsCertHook) Name() string                                  { return "tls_certificate" }
func (*tlsCertHook) Check(_, _ *config.Config) error               { return nil }
func (h *tlsCertHook) Apply(_ context.Context, _, _ *config.Config) (HookStatus, error) {
	if h.reloader == nil {
		return HookSkipped, nil
	}
	if err := h.reloader.Reload(); err != nil {
		return HookFailed, err
	}
	return HookApplied, nil
}

// -------------------------------------------------------------------------
// BUCKET CREDENTIALS
// -------------------------------------------------------------------------

// bucketAuthHook swaps the S3 server's bucket credentials registry.
type bucketAuthHook struct {
	inj do.Injector
}

func (*bucketAuthHook) Name() string                                  { return "bucket_credentials" }
func (*bucketAuthHook) Check(_, _ *config.Config) error               { return nil }
func (h *bucketAuthHook) Apply(_ context.Context, _, newCfg *config.Config) (HookStatus, error) {
	srv := optional[*s3api.Server](h.inj)
	if srv == nil {
		return HookSkipped, nil
	}
	srv.SetBucketAuth(auth.NewBucketRegistry(newCfg.Buckets))
	return HookApplied, nil
}

// -------------------------------------------------------------------------
// RATE LIMITER
// -------------------------------------------------------------------------

// rateLimitHook pushes new rate-limit knobs onto the limiter when the
// feature is currently enabled. A disabled limiter is intentionally
// left untouched - flipping enabled across reload requires a restart.
type rateLimitHook struct {
	inj do.Injector
}

func (*rateLimitHook) Name() string                                  { return "rate_limit" }
func (*rateLimitHook) Check(_, _ *config.Config) error               { return nil }
func (h *rateLimitHook) Apply(_ context.Context, _, newCfg *config.Config) (HookStatus, error) {
	if !newCfg.RateLimit.Enabled {
		return HookSkipped, nil
	}
	rl := optional[*s3api.RateLimiter](h.inj)
	if rl == nil {
		return HookSkipped, nil
	}
	rl.UpdateLimits(newCfg.RateLimit.RequestsPerSec, newCfg.RateLimit.Burst)
	return HookApplied, nil
}

// -------------------------------------------------------------------------
// QUOTA SYNC
// -------------------------------------------------------------------------

// quotaSyncHook re-runs SyncQuotaLimits against the new backend list.
type quotaSyncHook struct {
	inj do.Injector
}

func (*quotaSyncHook) Name() string                                  { return "quota_sync" }
func (*quotaSyncHook) Check(_, _ *config.Config) error               { return nil }
func (h *quotaSyncHook) Apply(ctx context.Context, _, newCfg *config.Config) (HookStatus, error) {
	admin := optional[core.LifecycleAdmin](h.inj)
	if admin == nil {
		return HookSkipped, nil
	}
	if err := admin.SyncQuotaLimits(ctx, newCfg.Backends); err != nil {
		return HookFailed, err
	}
	return HookApplied, nil
}

// -------------------------------------------------------------------------
// USAGE LIMITS
// -------------------------------------------------------------------------

// usageLimitsHook rebuilds the in-memory per-backend usage limit map
// and pushes it onto the manager.
type usageLimitsHook struct {
	inj do.Injector
}

func (*usageLimitsHook) Name() string                                  { return "usage_limits" }
func (*usageLimitsHook) Check(_, _ *config.Config) error               { return nil }
func (h *usageLimitsHook) Apply(_ context.Context, _, newCfg *config.Config) (HookStatus, error) {
	manager := optional[*proxy.BackendManager](h.inj)
	if manager == nil {
		return HookSkipped, nil
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
	return HookApplied, nil
}

// -------------------------------------------------------------------------
// LOG LEVEL
// -------------------------------------------------------------------------

// logLevelHook updates the slog level the runtime threaded in.
type logLevelHook struct {
	level *slog.LevelVar
}

func (*logLevelHook) Name() string                                  { return "log_level" }
func (*logLevelHook) Check(_, _ *config.Config) error               { return nil }
func (h *logLevelHook) Apply(_ context.Context, _, newCfg *config.Config) (HookStatus, error) {
	if h.level == nil {
		return HookSkipped, nil
	}
	h.level.Set(config.ParseLogLevel(newCfg.Server.LogLevel))
	return HookApplied, nil
}

// -------------------------------------------------------------------------
// WORKER CONFIGS
// -------------------------------------------------------------------------

// workerConfigsHook pushes new Rebalance/Replication/Integrity configs
// onto each registered worker. Workers not constructed in this run mode
// (e.g. api-only) silently skip; the hook reports Applied as long as
// at least one worker accepted the new config, Skipped otherwise.
type workerConfigsHook struct {
	inj do.Injector
}

func (*workerConfigsHook) Name() string                                  { return "worker_configs" }
func (*workerConfigsHook) Check(_, _ *config.Config) error               { return nil }
func (h *workerConfigsHook) Apply(_ context.Context, _, newCfg *config.Config) (HookStatus, error) {
	applied := 0
	if rb := optional[*worker.Rebalancer](h.inj); rb != nil {
		rb.SetConfig(&newCfg.Rebalance)
		applied++
	}
	if rp := optional[*worker.Replicator](h.inj); rp != nil {
		rp.SetConfig(&newCfg.Replication)
		applied++
	}
	if or := optional[*worker.OverReplicationCleaner](h.inj); or != nil {
		or.SetConfig(&newCfg.Replication)
		applied++
	}
	if sc := optional[*worker.Scrubber](h.inj); sc != nil {
		sc.SetConfig(&newCfg.Integrity)
		applied++
	}
	if applied == 0 {
		return HookSkipped, nil
	}
	return HookApplied, nil
}

// -------------------------------------------------------------------------
// MANAGER CONFIG SECTIONS
// -------------------------------------------------------------------------

// managerConfigHook updates UsageFlush, Lifecycle, and Integrity
// per-section configs on the manager and refreshes Prometheus quota
// gauges. The metrics refresh is the only fallible step; a failure
// there returns HookFailed but the AtomicConfig swaps already
// happened, matching historical best-effort semantics.
type managerConfigHook struct {
	inj do.Injector
}

func (*managerConfigHook) Name() string                                  { return "manager_config" }
func (*managerConfigHook) Check(_, _ *config.Config) error               { return nil }
func (h *managerConfigHook) Apply(ctx context.Context, _, newCfg *config.Config) (HookStatus, error) {
	manager := optional[*proxy.BackendManager](h.inj)
	if manager == nil {
		return HookSkipped, nil
	}
	manager.SetUsageFlushConfig(&newCfg.UsageFlush)
	manager.SetLifecycleConfig(&newCfg.Lifecycle)
	manager.SetIntegrityConfig(&newCfg.Integrity)
	if err := manager.UpdateQuotaMetrics(ctx); err != nil {
		return HookFailed, err
	}
	return HookApplied, nil
}

// -------------------------------------------------------------------------
// UI HANDLER
// -------------------------------------------------------------------------

// uiHandlerHook swaps the UI handler's config pointer.
type uiHandlerHook struct {
	inj do.Injector
}

func (*uiHandlerHook) Name() string                                  { return "ui_handler" }
func (*uiHandlerHook) Check(_, _ *config.Config) error               { return nil }
func (h *uiHandlerHook) Apply(_ context.Context, _, newCfg *config.Config) (HookStatus, error) {
	handler := optional[*ui.Handler](h.inj)
	if handler == nil {
		return HookSkipped, nil
	}
	handler.UpdateConfig(newCfg)
	return HookApplied, nil
}
