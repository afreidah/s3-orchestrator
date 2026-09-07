// -------------------------------------------------------------------------------
// Bucket Registry Assembly
//
// Author: Alex Freidah
//
// Resolves the provisioning store, merges it with what the config file declares,
// and builds the registry the request path authenticates against. The merge
// itself belongs to the provisioning package; this is only the wiring that hands
// it a store.
// -------------------------------------------------------------------------------

package di

import (
	"context"
	"log/slog"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/provisioning"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
)

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

	view, err := provisioning.LoadMerged(ctx, store, cfg.Buckets)
	if err != nil {
		return nil, err
	}

	registry, err := auth.NewBucketRegistry(&view)
	if err != nil {
		return nil, err
	}
	logAssemblyNotices(ctx, registry.Notices())
	return registry, nil
}

// RegistryPublisher rebuilds the request-time registry from what the store now
// holds and swaps it into the running server. The operations layer holds this
// so a credential issued through the provisioning API authenticates on the next
// request rather than the next restart.
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

// Republish reassembles the registry and installs it.
func (p *RegistryPublisher) Republish(ctx context.Context) error {
	cfg, err := do.Invoke[*config.Config](p.inj)
	if err != nil {
		return err
	}
	srv, err := do.Invoke[*s3api.Server](p.inj)
	if err != nil {
		return err
	}
	registry, err := AssembleBucketRegistry(ctx, p.inj, cfg)
	if err != nil {
		return err
	}
	srv.SetBucketAuth(registry)
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
