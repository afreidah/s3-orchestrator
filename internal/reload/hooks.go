// -------------------------------------------------------------------------------
// Reload Coordinator - Concrete Hooks
//
// Author: Alex Freidah
//
// One Hook implementation per subsystem that rebuilds state from a reload: TLS
// cert rotator, the provisioning view (bucket set, credentials, CORS rules),
// rate limiter, quota sync, per-backend usage limits, slog level, and the quota
// gauges. Settings a component reads straight from the running configuration
// need no hook. Each hook resolves its dependencies through the injector (or a
// coordinator-owned handle for the cert reloader) so the coordinator stays a
// plain orchestrator that does not import any individual subsystem.
// -------------------------------------------------------------------------------

package reload

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/di"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
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
		&provisioningHook{inj: inj},
		&rateLimitHook{inj: inj},
		&quotaSyncHook{inj: inj},
		&usageLimitsHook{inj: inj},
		&logLevelHook{level: logLevel},
		&quotaMetricsHook{inj: inj},
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
// PROVISIONING
// -------------------------------------------------------------------------

// provisioningHook rebuilds what the config file merged with the store
// yields: the declared bucket set, the credential registry and the CORS rules.
// Loading from the file alone would drop every bucket and credential created
// through the API on each reload.
type provisioningHook struct {
	inj do.Injector
}

func (*provisioningHook) Name() string { return "provisioning" }

// Check loads the replacement and throws it away. Loading publishes nothing,
// so an ambiguous credential, an unreadable CORS rule or an unreadable store
// aborts the reload with every running view untouched.
func (h *provisioningHook) Check(_, newCfg *config.Config) error {
	if newCfg == nil {
		return nil
	}
	_, err := di.LoadProvisioning(context.Background(), h.inj, newCfg)
	return err
}

// Apply loads and installs the replacement.
func (h *provisioningHook) Apply(ctx context.Context, _, newCfg *config.Config) (HookStatus, error) {
	if err := di.ApplyProvisioning(ctx, h.inj, newCfg); err != nil {
		return HookFailed, err
	}
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
// QUOTA METRICS
// -------------------------------------------------------------------------

// quotaMetricsHook refreshes the quota gauges, which the new limits and
// replication factor change.
type quotaMetricsHook struct {
	inj do.Injector
}

func (*quotaMetricsHook) Name() string                    { return "quota_metrics" }
func (*quotaMetricsHook) Check(_, _ *config.Config) error { return nil }
func (h *quotaMetricsHook) Apply(ctx context.Context, _, _ *config.Config) (HookStatus, error) {
	return applyResolved(h.inj, "backend runtime", func(rt *infra.BackendRuntime) error {
		return rt.UpdateQuotaMetrics(ctx)
	})
}
