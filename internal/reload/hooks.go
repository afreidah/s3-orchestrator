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
	"github.com/afreidah/s3-orchestrator/internal/proxy/expiry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/proxy/usage"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
	"github.com/afreidah/s3-orchestrator/internal/transport/cors"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
	"github.com/afreidah/s3-orchestrator/internal/transport/ui"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// resolutionError renders an Optional[T] Failed outcome into the error
// shape reload hooks surface to the coordinator. The label is the human
// name of the subsystem so the resulting message reads naturally in
// admin status output.
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
func (*bucketAuthHook) Check(_, newCfg *config.Config) error {
	if newCfg == nil {
		return nil
	}
	_, err := auth.NewBucketRegistry(newCfg.Buckets)
	return err
}

func (h *bucketAuthHook) Apply(_ context.Context, _, newCfg *config.Config) (HookStatus, error) {
	res := di.Optional[*s3api.Server](h.inj)
	if res.Failed() {
		return HookFailed, resolutionError("S3 server", res.Err)
	}
	if res.Value == nil {
		return HookSkipped, nil
	}
	registry, err := auth.NewBucketRegistry(newCfg.Buckets)
	if err != nil {
		return HookFailed, err
	}
	res.Value.SetBucketAuth(registry)
	return HookApplied, nil
}

// -------------------------------------------------------------------------
// BUCKET CORS
// -------------------------------------------------------------------------

// corsHook swaps the compiled browser CORS rule set.
type corsHook struct {
	inj do.Injector
}

func (*corsHook) Name() string { return "bucket_cors" }

// Check compiles the replacement rule set and discards it, so a rule the
// matcher cannot read aborts the reload before any hook has applied
// anything and the running server keeps the rules it already had.
func (*corsHook) Check(_, newCfg *config.Config) error {
	if newCfg == nil {
		return nil
	}
	_, err := cors.NewRegistry(newCfg.Buckets)
	return err
}

func (h *corsHook) Apply(_ context.Context, _, newCfg *config.Config) (HookStatus, error) {
	res := di.Optional[*cors.Policy](h.inj)
	if res.Failed() {
		return HookFailed, resolutionError("CORS policy", res.Err)
	}
	if res.Value == nil {
		return HookSkipped, nil
	}
	rules, err := cors.NewRegistry(newCfg.Buckets)
	if err != nil {
		return HookFailed, err
	}
	res.Value.SetRules(rules)
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

func (*rateLimitHook) Name() string                    { return "rate_limit" }
func (*rateLimitHook) Check(_, _ *config.Config) error { return nil }
func (h *rateLimitHook) Apply(_ context.Context, _, newCfg *config.Config) (HookStatus, error) {
	if !newCfg.RateLimit.Enabled {
		return HookSkipped, nil
	}
	res := di.Optional[*s3api.RateLimiter](h.inj)
	if res.Failed() {
		return HookFailed, resolutionError("rate limiter", res.Err)
	}
	if res.Value == nil {
		return HookSkipped, nil
	}
	res.Value.UpdateLimits(newCfg.RateLimit.RequestsPerSec, newCfg.RateLimit.Burst)
	return HookApplied, nil
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
	res := di.Optional[core.LifecycleAdmin](h.inj)
	if res.Failed() {
		return HookFailed, resolutionError("lifecycle admin", res.Err)
	}
	if res.Value == nil {
		return HookSkipped, nil
	}
	if err := res.Value.SyncQuotaLimits(ctx, newCfg.Backends); err != nil {
		return HookFailed, err
	}
	return HookApplied, nil
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
	res := di.Optional[*infra.BackendRuntime](h.inj)
	if res.Failed() {
		return HookFailed, resolutionError("backend runtime", res.Err)
	}
	if res.Value == nil {
		return HookSkipped, nil
	}
	limits := make(map[string]core.UsageLimits, len(newCfg.Backends))
	for i := range newCfg.Backends {
		bcfg := &newCfg.Backends[i]
		lim, err := di.UsageLimitsFor(bcfg)
		if err != nil {
			return HookFailed, fmt.Errorf("backend %s: %w", bcfg.Name, err)
		}
		limits[bcfg.Name] = lim
	}
	var applier usageLimitsApplier = res.Value.Usage()
	applier.UpdateLimits(limits)
	return HookApplied, nil
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
	res := di.Optional[*ops.Services](h.inj)
	if res.Failed() {
		return HookFailed, resolutionError("operations layer", res.Err)
	}
	if res.Value == nil {
		return HookSkipped, nil
	}
	res.Value.UpdateConfig(newCfg)
	return HookApplied, nil
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
	res := di.Optional[*ui.Handler](h.inj)
	if res.Failed() {
		return HookFailed, resolutionError("UI handler", res.Err)
	}
	if res.Value == nil {
		return HookSkipped, nil
	}
	res.Value.UpdateConfig(newCfg)
	return HookApplied, nil
}
