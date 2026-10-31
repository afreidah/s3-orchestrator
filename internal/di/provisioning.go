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
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/provisioning"
	"github.com/afreidah/s3-orchestrator/internal/sharedstate"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
	"github.com/afreidah/s3-orchestrator/internal/transport/cors"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
)

// provisioningChannel is the shared-state channel provisioning changes are
// announced on.
const provisioningChannel = "provisioning"

// provisioningTopic is the topic a provisioning change is announced on. The
// message carries nothing: a receiver rebuilds its whole view from the store.
func provisioningTopic(s *sharedstate.Shared) sharedstate.Topic[struct{}] {
	return sharedstate.NewTopic[struct{}](s, provisioningChannel)
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Provisioned is everything built from the provisioning view: the declared
// bucket set, the credential registry the request path authenticates against,
// and the compiled CORS rules. Building it changes nothing; Install publishes it.
type Provisioned struct {
	buckets  []provisioning.Bucket
	registry *auth.BucketRegistry
	cors     *cors.Registry
}

// LoadProvisioning builds the provisioning state from both sources a
// deployment declares buckets in: the buckets in cfg and what the store holds.
// Loading from config alone would drop every API-created bucket. An unreadable
// store is an error rather than a fallback to config, which would answer 403
// to entitled callers; so is an ambiguous credential or an unreadable CORS rule.
func LoadProvisioning(ctx context.Context, i do.Injector, cfg *config.Config) (*Provisioned, error) {
	store, err := do.Invoke[core.ProvisioningStore](i)
	if err != nil {
		return nil, err
	}
	view, err := provisioning.LoadMerged(ctx, store, cfg.Buckets, cfg.Auth)
	if err != nil {
		return nil, err
	}
	p := &Provisioned{buckets: view.Buckets}
	if p.registry, err = auth.NewBucketRegistry(&view); err != nil {
		return nil, err
	}
	if p.cors, err = cors.NewRegistry(view.Buckets); err != nil {
		return nil, err
	}
	return p, nil
}

// Install publishes p on this instance: the declared bucket set first, then,
// in a mode that serves the S3 API, the CORS rules and the credential registry.
// The order means no bucket is reachable over S3 while the admin endpoints,
// the CORS policy and the reconciler still believe it does not exist.
func (p *Provisioned) Install(ctx context.Context, i do.Injector) error {
	r := newResolver(i)
	declared := r.Resolve[*provisioning.Declared]()
	mode := r.Resolve[config.Mode]()
	if r.err != nil {
		return r.err
	}
	declared.Set(p.buckets)
	logAssemblyNotices(ctx, p.registry.Notices())
	if !mode.IsAPI() {
		return nil
	}
	srv := r.Resolve[*s3api.Server]()
	policy := r.Resolve[*cors.Policy]()
	if r.err != nil {
		return r.err
	}
	policy.SetRules(p.cors)
	srv.SetBucketAuth(p.registry)
	return nil
}

// ApplyProvisioning loads the provisioning state from cfg and the store and
// installs it, which is what a reload and a provisioning change both do.
func ApplyProvisioning(ctx context.Context, i do.Injector, cfg *config.Config) error {
	p, err := LoadProvisioning(ctx, i, cfg)
	if err != nil {
		return err
	}
	return p.Install(ctx, i)
}

// applyLiveProvisioning applies the provisioning state from the configuration
// in force now, which a reload may have replaced since startup.
func applyLiveProvisioning(ctx context.Context, i do.Injector) error {
	cfg, err := do.Invoke[*ops.ConfigStore](i)
	if err != nil {
		return err
	}
	return ApplyProvisioning(ctx, i, cfg.Load())
}

// RegistryPublisher rebuilds everything assembled from the provisioning view
// after a change through the provisioning API, on this instance and, through
// Redis, on every other, so a newly issued credential authenticates on any
// instance without a restart. Dependencies are resolved inside Republish
// because the server it swaps into is built from the registry this replaces,
// and resolving it at construction would create a provider cycle.
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
	if err := applyLiveProvisioning(ctx, p.inj); err != nil {
		return err
	}
	p.announce(ctx)
	return nil
}

// announce publishes a provisioning change to the other instances. A no-op on
// a single instance, which has no one to tell.
func (p *RegistryPublisher) announce(ctx context.Context) {
	shared, err := do.Invoke[*sharedstate.Shared](p.inj)
	if err == nil {
		err = provisioningTopic(shared).Publish(ctx, struct{}{})
	}
	if err != nil {
		slog.WarnContext(ctx, "provisioning change not announced to other instances",
			logfmt.Component("di"), logfmt.Err(err))
	}
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
