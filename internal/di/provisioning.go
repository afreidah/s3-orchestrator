// -------------------------------------------------------------------------------
// Bucket Registry Assembly
//
// Author: Alex Freidah
//
// Resolves the provisioning store, merges it with what the config file declares,
// and builds the registry the request path authenticates against. The merge
// itself belongs to the provisioning package; this is only the wiring that hands
// it a store, installs the result, and carries a change to the other instances.
// -------------------------------------------------------------------------------

package di

import (
	"context"
	"log/slog"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/provisioning"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
	"github.com/afreidah/s3-orchestrator/internal/transport/cors"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
)

// provisioningChannel is the Redis channel provisioning changes are announced
// on.
const provisioningChannel = "provisioning"

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// AssembleBucketRegistry builds the bucket registry from both sources a
// deployment declares credentials in: the buckets in cfg, and the users the
// store holds. Exported because the reload hook has to assemble the same way a
// boot does - rebuilding from the config file alone would drop every
// API-created bucket on each SIGHUP.
//
// A store that cannot be read fails rather than falling back to config alone:
// serving with half the credentials answers 403 to callers that are entitled,
// which is worse than not starting.
func AssembleBucketRegistry(ctx context.Context, i do.Injector, cfg *config.Config) (*auth.BucketRegistry, error) {
	store, err := do.Invoke[core.ProvisioningStore](i)
	if err != nil {
		return nil, err
	}

	declared, err := do.Invoke[*provisioning.Declared](i)
	if err != nil {
		return nil, err
	}

	view, err := provisioning.LoadMerged(ctx, store, cfg.Buckets, cfg.Auth)
	if err != nil {
		return nil, err
	}

	registry, err := auth.NewBucketRegistry(&view)
	if err != nil {
		return nil, err
	}

	// Published before the registry is returned, so nothing can observe a
	// bucket as reachable over S3 while the admin endpoints, the CORS policy
	// and the reconciler still believe it does not exist.
	declared.Set(view.Buckets)

	logAssemblyNotices(ctx, registry.Notices())
	return registry, nil
}

// RegistryPublisher rebuilds everything assembled from the provisioning view
// after a change through the provisioning API, on this instance and, through
// Redis, on every other. The operations layer holds this so a credential
// issued on one instance authenticates on the next request to any of them
// rather than after the next restart.
//
// Everything is resolved inside Republish rather than captured at construction:
// the server it swaps into is built from the registry this replaces, and
// resolving it eagerly would order the two providers against each other.
type RegistryPublisher struct {
	inj do.Injector
}

// NewRegistryPublisher builds the publisher over an injector.
func NewRegistryPublisher(i do.Injector) *RegistryPublisher {
	return &RegistryPublisher{inj: i}
}

// Republish rebuilds this instance's view, then announces the change so the
// other instances rebuild theirs. A failed rebuild is returned; a failed
// announcement is only logged, because the change is stored and live here,
// and an instance that missed it rebuilds when its subscription reconnects.
func (p *RegistryPublisher) Republish(ctx context.Context) error {
	if err := applyProvisioning(ctx, p.inj); err != nil {
		return err
	}
	p.announce(ctx)
	return nil
}

// announce publishes a provisioning change to the other instances. A no-op on
// a single instance, which has no Redis and no one to tell.
func (p *RegistryPublisher) announce(ctx context.Context) {
	cfg, err := do.Invoke[*config.Config](p.inj)
	if err != nil || cfg.Redis == nil {
		return
	}
	channel, err := do.Invoke[*counter.RedisCounterBackend](p.inj)
	if err == nil {
		err = channel.NotifyShared(ctx, provisioningChannel)
	}
	if err != nil {
		slog.WarnContext(ctx, "provisioning change not announced to other instances",
			logfmt.Component("di"), logfmt.Err(err))
	}
}

// applyProvisioning rebuilds, on this instance, everything assembled from the
// provisioning view: the declared bucket set and, in a mode that serves the
// S3 API, the request-time registry and the compiled CORS rules.
func applyProvisioning(ctx context.Context, i do.Injector) error {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return err
	}
	mode, err := do.InvokeNamed[config.Mode](i, "mode")
	if err != nil {
		return err
	}
	registry, err := AssembleBucketRegistry(ctx, i, cfg)
	if err != nil {
		return err
	}
	if !mode.IsAPI() {
		return nil
	}

	srv, err := do.Invoke[*s3api.Server](i)
	if err != nil {
		return err
	}
	policy, err := do.Invoke[*cors.Policy](i)
	if err != nil {
		return err
	}
	declared, err := do.Invoke[*provisioning.Declared](i)
	if err != nil {
		return err
	}
	rules, err := cors.NewRegistry(declared.Buckets())
	if err != nil {
		return err
	}
	srv.SetBucketAuth(registry)
	policy.SetRules(rules)
	return nil
}

// logAssemblyNotices reports what assembly served through. Warn rather than
// error: each one describes a state the fleet is running in, not a failure to
// reach it.
func logAssemblyNotices(ctx context.Context, notices []provisioning.Notice) {
	for _, n := range notices {
		slog.WarnContext(ctx, "bucket registry assembly",
			logfmt.Component("di"),
			"kind", n.Kind,
			"detail", n.Detail,
		)
	}
}
