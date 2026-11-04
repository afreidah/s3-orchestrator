// -------------------------------------------------------------------------------
// DI - Database and Store Providers
//
// Author: Alex Freidah
//
// Wires the metadata store and its narrow role aliases. ProvideMetadataStore
// is the only place a concrete driver package (postgres / sqlite) is
// imported; every consumer downstream sees the engine-agnostic core
// interfaces. Each driver wraps its own DBTX/DB chokepoint with the
// shared *breaker.CircuitBreaker, so role aliases do not re-wrap.
// -------------------------------------------------------------------------------

package di

import (
	"context"
	"log/slog"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/instanceid"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"github.com/afreidah/s3-orchestrator/internal/store/engine"
	"github.com/afreidah/s3-orchestrator/internal/store/postgres"
	sqlitestore "github.com/afreidah/s3-orchestrator/internal/store/sqlite"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// metadataStore exists only so the composition root can carry one opened engine
// to the do.MustAs role aliases in injector.go. It stays unexported so consumers
// depend on individual roles, never on the union.
type metadataStore interface {
	engine.Store
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// provideMetadataStore opens the configured driver, runs migrations, syncs
// quota limits, and returns the opened engine for the role aliases to split.
// CB protection lives inside each driver's DBTX/DB chokepoint, so this
// provider does no wrapping of its own.
func provideMetadataStore(i do.Injector) (metadataStore, error) {
	r := NewResolver(i)
	cfg := r.Resolve[*config.Config]()
	cb := r.Resolve[*breaker.CircuitBreaker]()
	id := r.Resolve[instanceid.ID]()
	if r.err != nil {
		return nil, r.err
	}
	ctx := context.Background()

	cs, err := store.Open(ctx, &cfg.Database, cb, id.String())
	if err != nil {
		return nil, err
	}
	if err := store.Prepare(ctx, cs, cfg.Backends); err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "database migrations applied",
		logfmt.Component("di"),
		"driver", cfg.Database.Driver,
	)
	return cs, nil
}

// ProvideDatabaseBreaker constructs the shared *breaker.CircuitBreaker every
// driver-level SQL statement forwards calls through, reading errors the way
// the configured driver reports them.
func ProvideDatabaseBreaker(i do.Injector) (*breaker.CircuitBreaker, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	requestError := postgres.RequestError
	if cfg.Database.Driver == "sqlite" {
		requestError = sqlitestore.RequestError
	}
	return store.NewDatabaseBreaker(cfg.CircuitBreaker, requestError), nil
}

// ProvideInstanceID resolves this process's identifier, generated once at
// first invoke and reused everywhere the value is needed.
func ProvideInstanceID(_ do.Injector) (instanceid.ID, error) {
	return instanceid.New(), nil
}
