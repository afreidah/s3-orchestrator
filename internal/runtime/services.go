// -------------------------------------------------------------------------------
// Runtime - Service Resolution
//
// Author: Alex Freidah
//
// Forces eager construction of the services the daemon needs available before
// the HTTP server starts accepting traffic. Required services bail with a
// startup error; optional services are constructed lazily on first use by
// their consumers. Worker config application is colocated here because it
// runs immediately after the workers' providers fire.
// -------------------------------------------------------------------------------

package runtime

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/di"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/proxy/multipart"
	"github.com/afreidah/s3-orchestrator/internal/proxy/object"
	"github.com/afreidah/s3-orchestrator/internal/proxy/usage"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// bootstrapped is what the resolve pass hands back: the collaborators the
// runtime keeps hold of after startup, either to seed with config or to drive
// in order during shutdown. Everything else the injector owns and disposes of
// itself.
type bootstrapped struct {
	objects      *object.Manager
	multipart    *multipart.Manager
	usage        *usage.Service
	runtime      *infra.BackendRuntime
	integrityCfg *syncutil.AtomicConfig[config.IntegrityConfig]
}

// resolveBootstrapped builds the backend stack and returns the pieces startup
// and shutdown hold onto. Resolving them here rather than at each use keeps a
// missing provider a startup error instead of a nil dereference at shutdown,
// when there is nothing left to report it to.
func resolveBootstrapped(inj do.Injector) (bootstrapped, error) {
	var b bootstrapped
	var err error
	if b.runtime, err = do.Invoke[*infra.BackendRuntime](inj); err != nil {
		return b, fmt.Errorf("initialize backend runtime: %w", err)
	}
	if b.objects, err = do.Invoke[*object.Manager](inj); err != nil {
		return b, fmt.Errorf("initialize object manager: %w", err)
	}
	if b.multipart, err = do.Invoke[*multipart.Manager](inj); err != nil {
		return b, fmt.Errorf("initialize multipart manager: %w", err)
	}
	if b.usage, err = do.Invoke[*usage.Service](inj); err != nil {
		return b, fmt.Errorf("initialize usage service: %w", err)
	}
	if b.integrityCfg, err = do.Invoke[*syncutil.AtomicConfig[config.IntegrityConfig]](inj); err != nil {
		return b, fmt.Errorf("initialize integrity config: %w", err)
	}
	return b, nil
}

// resolveRequiredServices triggers eager construction of every dependency
// that must exist before the HTTP server starts. Optional services
// (rate limiter, UI throttle) only resolve when their feature flag is
// set. Each step returns a wrapped error so the failure source is clear.
func resolveRequiredServices(inj do.Injector, cfg *config.Config) (bootstrapped, error) {
	var b bootstrapped
	if _, err := do.Invoke[core.LifecycleAdmin](inj); err != nil {
		return b, fmt.Errorf("initialize database: %w", err)
	}
	if _, err := do.Invoke[*breaker.CircuitBreaker](inj); err != nil {
		return b, fmt.Errorf("initialize database circuit breaker: %w", err)
	}
	b, err := resolveBootstrapped(inj)
	if err != nil {
		return b, err
	}
	if err := di.WireManager(inj); err != nil {
		return b, fmt.Errorf("wire backend stack: %w", err)
	}
	if _, err := do.Invoke[*s3api.Server](inj); err != nil {
		return b, fmt.Errorf("initialize S3 server: %w", err)
	}

	if err := applyWorkerConfigs(inj, &cfg.Rebalance, &cfg.Replication, &cfg.Integrity); err != nil {
		return b, err
	}
	b.usage.SetConfig(&cfg.UsageFlush)
	b.integrityCfg.Store(&cfg.Integrity)
	// Lifecycle config is seeded by ProvideExpiryManager, which owns it.

	if _, err := do.Invoke[*lifecycle.Manager](inj); err != nil {
		return b, fmt.Errorf("initialize lifecycle manager: %w", err)
	}

	if cfg.RateLimit.Enabled {
		if _, err := do.Invoke[*s3api.RateLimiter](inj); err != nil {
			return b, fmt.Errorf("initialize rate limiter: %w", err)
		}
	}
	if cfg.UI.Enabled {
		if _, err := do.Invoke[*httputil.LoginThrottle](inj); err != nil {
			return b, fmt.Errorf("initialize login throttle: %w", err)
		}
	}

	ctx := context.Background()
	if err := b.runtime.UpdateQuotaMetrics(ctx); err != nil {
		slog.WarnContext(ctx, "initial quota metrics refresh failed",
			logfmt.Component("runtime"),
			"error", err,
		)
	}
	return b, nil
}

// applyWorkerConfigs pushes Rebalance / Replication / Integrity configs
// onto each worker. Workers that did not get constructed (e.g. api-only
// mode) silently skip. Errors are returned only for unexpected wiring
// failures; "worker not registered" is normal.
func applyWorkerConfigs(inj do.Injector, rebalance *config.RebalanceConfig, replication *config.ReplicationConfig, integrity *config.IntegrityConfig) error {
	if rb, err := do.Invoke[*worker.Rebalancer](inj); err == nil {
		rb.SetConfig(rebalance)
	}
	if rp, err := do.Invoke[*worker.Replicator](inj); err == nil {
		rp.SetConfig(replication)
		rp.SetIntegrityConfig(integrity)
	}
	if or, err := do.Invoke[*worker.OverReplicationCleaner](inj); err == nil {
		or.SetConfig(replication)
	}
	if sc, err := do.Invoke[*worker.Scrubber](inj); err == nil {
		sc.SetConfig(integrity)
	}
	return nil
}
