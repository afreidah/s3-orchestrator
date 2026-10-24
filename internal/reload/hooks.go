// -------------------------------------------------------------------------------
// Reload Coordinator - Concrete Hooks
//
// Author: Alex Freidah
//
// One Hook implementation per reloadable subsystem: TLS cert rotator,
// S3 bucket auth registry, browser CORS rules, rate limiter, quota sync,
// per-backend usage limits, slog level, worker configs, manager config
// sections, and the UI handler. Each hook resolves its dependencies through
// the injector (or a coordinator-owned handle for the cert reloader) so the
// coordinator stays a plain orchestrator that does not import any individual
// subsystem.
// -------------------------------------------------------------------------------

package reload

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/di"
	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/provisioning"
	"github.com/afreidah/s3-orchestrator/internal/proxy/expiry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/proxy/usage"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/cors"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
	"github.com/afreidah/s3-orchestrator/internal/transport/ui"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// resolutionError wraps a failed Optional[T] resolution with the subsystem's
// label for reload status output.
func resolutionError(label string, err error) error {
	return fmt.Errorf("%s resolution failed: %w", label, err)
}

// defaultHooks returns the standard set of reload hooks the runtime wires in,
// in the order they must run. The cert reloader is passed directly because it
// is not registered in the injector.
func defaultHooks(inj do.Injector, certReloader *httputil.CertReloader, logLevel *slog.LevelVar) []Hook {
	return []Hook{
		&tlsCertHook{reloader: certReloader},
		&bucketAuthHook{inj: inj},
		&corsHook{inj: inj},
		&rateLimitHook{inj: inj},
		&quotaSyncHook{inj: inj},
		&usageLimitsHook{inj: inj},
		&logLevelHook{level: logLevel},
		&workerConfigsHook{inj: inj},
		&runtimeConfigHook{inj: inj},
		&opsHook{inj: inj},
		&uiHandlerHook{inj: inj},
	}
}

// applyResolved resolves T and hands it to apply. A provider that failed to
// build fails the hook, one this run mode never registered skips it, and
// otherwise apply's error decides between applied and failed.
func applyResolved[T any](inj do.Injector, what string, apply func(T) error) (HookStatus, error) {
	res := di.Optional[T](inj)
	switch res.Resolution {
	case di.ResolutionFailed:
		return HookFailed, resolutionError(what, res.Err)
	case di.ResolutionDisabled:
		return HookSkipped, nil
	}
	if err := apply(res.Value); err != nil {
		return HookFailed, err
	}
	return HookApplied, nil
}

// -------------------------------------------------------------------------
// TLS CERTIFICATE
// -------------------------------------------------------------------------

// tlsCertHook rotates the listener's TLS certificate by re-reading
// the cert/key pair from disk. Skipped when TLS is not configured.
type tlsCertHook struct {
	reloader *httputil.CertReloader
}

func (*tlsCertHook) Name() string                    { return "tls_certificate" }
func (*tlsCertHook) Check(_, _ *config.Config) error { return nil }
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

func (*bucketAuthHook) Name() string { return "bucket_credentials" }

// Check builds the replacement registry and throws it away. Doing it here
// means an ambiguous credential aborts the whole reload before any hook has
// applied anything, so the running server keeps the registry it already had.
func (h *bucketAuthHook) Check(_, newCfg *config.Config) error {
	if newCfg == nil {
		return nil
	}
	_, err := di.AssembleBucketRegistry(context.Background(), h.inj, newCfg)
	return err
}

// Apply rebuilds from the config file merged with the store, which is what a
// boot does. Rebuilding from the file alone would drop every bucket and
// credential created through the API on each reload.
func (h *bucketAuthHook) Apply(ctx context.Context, _, newCfg *config.Config) (HookStatus, error) {
	return applyResolved(h.inj, "S3 server", func(srv *s3api.Server) error {
		registry, err := di.AssembleBucketRegistry(ctx, h.inj, newCfg)
		if err != nil {
			return err
		}
		srv.SetBucketAuth(registry)
		return nil
	})
}

// -------------------------------------------------------------------------
// BUCKET CORS
// -------------------------------------------------------------------------

// corsHook swaps the compiled browser CORS rule set.
type corsHook struct {
	inj do.Injector
}

func (*corsHook) Name() string { return "bucket_cors" }

// Check compiles the incoming file's rules and discards them, so a rule the
// matcher cannot read aborts the reload before any hook has applied anything
// and the running server keeps the rules it already had. Only the file's rules
// are checked; the store's were compiled when the provisioning API accepted
// them.
func (*corsHook) Check(_, newCfg *config.Config) error {
	if newCfg == nil {
		return nil
	}
	_, err := cors.NewRegistry(provisioning.Merge(newCfg.Buckets, newCfg.Auth, &provisioning.Snapshot{}).Buckets)
	return err
}

// Apply compiles from the merged set bucketAuthHook has just republished, so a
// bucket created through the provisioning API keeps its rules across a reload
// instead of having them dropped back to whatever the file declares.
func (h *corsHook) Apply(_ context.Context, _, _ *config.Config) (HookStatus, error) {
	return applyResolved(h.inj, "CORS policy", func(policy *cors.Policy) error {
		declared := di.Optional[*provisioning.Declared](h.inj)
		if declared.Failed() {
			return resolutionError("declared buckets", declared.Err)
		}
		rules, err := cors.NewRegistry(declared.Value.Buckets())
		if err != nil {
			return err
		}
		policy.SetRules(rules)
		return nil
	})
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

func (*rateLimitHook) Name() string                    { return "rate_limit" }
func (*rateLimitHook) Check(_, _ *config.Config) error { return nil }
func (h *rateLimitHook) Apply(_ context.Context, _, newCfg *config.Config) (HookStatus, error) {
	if !newCfg.RateLimit.Enabled {
		return HookSkipped, nil
	}
	return applyResolved(h.inj, "rate limiter", func(rl *s3api.RateLimiter) error {
		rl.UpdateLimits(newCfg.RateLimit.RequestsPerSec, newCfg.RateLimit.Burst)
		return nil
	})
}

// -------------------------------------------------------------------------
// QUOTA SYNC
// -------------------------------------------------------------------------

// quotaSyncHook re-runs SyncQuotaLimits against the new backend list.
type quotaSyncHook struct {
	inj do.Injector
}

func (*quotaSyncHook) Name() string                    { return "quota_sync" }
func (*quotaSyncHook) Check(_, _ *config.Config) error { return nil }
func (h *quotaSyncHook) Apply(ctx context.Context, _, newCfg *config.Config) (HookStatus, error) {
	return applyResolved(h.inj, "lifecycle admin", func(admin core.LifecycleAdmin) error {
		return admin.SyncQuotaLimits(ctx, newCfg.Backends)
	})
}

// -------------------------------------------------------------------------
// USAGE LIMITS
// -------------------------------------------------------------------------

// usageLimitsHook rebuilds the in-memory per-backend usage limit map
// and pushes it onto the usage tracker.
type usageLimitsHook struct {
	inj do.Injector
}

func (*usageLimitsHook) Name() string                    { return "usage_limits" }
func (*usageLimitsHook) Check(_, _ *config.Config) error { return nil }
func (h *usageLimitsHook) Apply(_ context.Context, _, newCfg *config.Config) (HookStatus, error) {
	return applyResolved(h.inj, "backend runtime", func(rt *infra.BackendRuntime) error {
		limits := make(map[string]core.UsageLimits, len(newCfg.Backends))
		for i := range newCfg.Backends {
			bcfg := &newCfg.Backends[i]
			lim, err := di.UsageLimitsFor(bcfg)
			if err != nil {
				return fmt.Errorf("backend %s: %w", bcfg.Name, err)
			}
			limits[bcfg.Name] = lim
		}
		var applier usageLimitsApplier = rt.Usage()
		applier.UpdateLimits(limits)
		return nil
	})
}

// -------------------------------------------------------------------------
// LOG LEVEL
// -------------------------------------------------------------------------

// logLevelHook updates the slog level the runtime threaded in.
type logLevelHook struct {
	level *slog.LevelVar
}

func (*logLevelHook) Name() string                    { return "log_level" }
func (*logLevelHook) Check(_, _ *config.Config) error { return nil }
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

func (*workerConfigsHook) Name() string                    { return "worker_configs" }
func (*workerConfigsHook) Check(_, _ *config.Config) error { return nil }
func (h *workerConfigsHook) Apply(_ context.Context, _, newCfg *config.Config) (HookStatus, error) {
	rbRes := di.Optional[*worker.Rebalancer](h.inj)
	if rbRes.Failed() {
		return HookFailed, resolutionError("rebalancer", rbRes.Err)
	}
	rpRes := di.Optional[*worker.Replicator](h.inj)
	if rpRes.Failed() {
		return HookFailed, resolutionError("replicator", rpRes.Err)
	}
	orRes := di.Optional[*worker.OverReplicationCleaner](h.inj)
	if orRes.Failed() {
		return HookFailed, resolutionError("over-replication cleaner", orRes.Err)
	}
	scRes := di.Optional[*worker.Scrubber](h.inj)
	if scRes.Failed() {
		return HookFailed, resolutionError("scrubber", scRes.Err)
	}
	applied := 0
	if rbRes.Value != nil {
		rbRes.Value.SetConfig(&newCfg.Rebalance)
		applied++
	}
	if rpRes.Value != nil {
		rpRes.Value.SetConfig(&newCfg.Replication)
		rpRes.Value.SetIntegrityConfig(&newCfg.Integrity)
		applied++
	}
	if orRes.Value != nil {
		orRes.Value.SetConfig(&newCfg.Replication)
		applied++
	}
	if scRes.Value != nil {
		scRes.Value.SetConfig(&newCfg.Integrity)
		applied++
	}
	if applied == 0 {
		return HookSkipped, nil
	}
	return HookApplied, nil
}

// -------------------------------------------------------------------------
// RELOADABLE CONFIG SECTIONS
// -------------------------------------------------------------------------

// runtimeConfigHook updates the UsageFlush, Lifecycle, and Integrity
// per-section configs on the collaborators that read them, and refreshes
// Prometheus quota gauges. The metrics refresh is the only fallible step, and
// it is best-effort: a failure there returns HookFailed, but the AtomicConfig
// swaps have already happened and are not rolled back.
type runtimeConfigHook struct {
	inj do.Injector
}

func (*runtimeConfigHook) Name() string                    { return "runtime_config" }
func (*runtimeConfigHook) Check(_, _ *config.Config) error { return nil }
func (h *runtimeConfigHook) Apply(ctx context.Context, _, newCfg *config.Config) (HookStatus, error) {
	usageRes := di.Optional[*usage.Service](h.inj)
	if usageRes.Failed() {
		return HookFailed, resolutionError("usage service", usageRes.Err)
	}
	integrityRes := di.Optional[*syncutil.AtomicConfig[config.IntegrityConfig]](h.inj)
	if integrityRes.Failed() {
		return HookFailed, resolutionError("integrity config", integrityRes.Err)
	}
	fleetRes := di.Optional[*infra.BackendRuntime](h.inj)
	if fleetRes.Failed() {
		return HookFailed, resolutionError("backend runtime", fleetRes.Err)
	}
	if usageRes.Value == nil || integrityRes.Value == nil || fleetRes.Value == nil {
		return HookSkipped, nil
	}

	var flushApplier usageFlushConfigApplier = usageRes.Value
	flushApplier.SetConfig(&newCfg.UsageFlush)
	integrityRes.Value.Store(&newCfg.Integrity)

	// Lifecycle rules live with the code that applies them.
	if exp := di.Optional[*expiry.Manager](h.inj); exp.Failed() {
		return HookFailed, resolutionError("expiry manager", exp.Err)
	} else if exp.Value != nil {
		var lifecycleApplier lifecycleConfigApplier = exp.Value
		lifecycleApplier.SetConfig(&newCfg.Lifecycle)
	}

	var refresher quotaMetricsRefresher = fleetRes.Value
	if err := refresher.UpdateQuotaMetrics(ctx); err != nil {
		return HookFailed, err
	}
	return HookApplied, nil
}

// -------------------------------------------------------------------------
// OPERATIONS LAYER
// -------------------------------------------------------------------------

// opsHook swaps the config every operation reads, so a manual run started
// after a reload uses the settings now in force.
type opsHook struct {
	inj do.Injector
}

func (*opsHook) Name() string                    { return "ops" }
func (*opsHook) Check(_, _ *config.Config) error { return nil }
func (h *opsHook) Apply(_ context.Context, _, newCfg *config.Config) (HookStatus, error) {
	return applyResolved(h.inj, "operations layer", func(svc *ops.Services) error {
		svc.UpdateConfig(newCfg)
		return nil
	})
}

// -------------------------------------------------------------------------
// UI HANDLER
// -------------------------------------------------------------------------

// uiHandlerHook swaps the UI handler's config pointer.
type uiHandlerHook struct {
	inj do.Injector
}

func (*uiHandlerHook) Name() string                    { return "ui_handler" }
func (*uiHandlerHook) Check(_, _ *config.Config) error { return nil }
func (h *uiHandlerHook) Apply(_ context.Context, _, newCfg *config.Config) (HookStatus, error) {
	return applyResolved(h.inj, "UI handler", func(handler *ui.Handler) error {
		handler.UpdateConfig(newCfg)
		return nil
	})
}
