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
	"fmt"
	"log/slog"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/instanceid"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/postgres"
	sqlitestore "github.com/afreidah/s3-orchestrator/internal/store/sqlite"
)

// ProvideMetadataStore opens the configured driver, runs migrations,
// syncs quota limits, and returns the wide core.MetadataStore every
// consumer depends on. CB protection lives inside each driver's DBTX/DB
// chokepoint, so this provider does no wrapping of its own.
func ProvideMetadataStore(i do.Injector) (core.MetadataStore, error) {
	r := newResolver(i)
	cfg := resolve[*config.Config](r)
	cb := resolve[*breaker.CircuitBreaker](r)
	if r.err != nil {
		return nil, r.err
	}
	ctx := context.Background()

	cs, err := openStore(ctx, &cfg.Database, cb)
	if err != nil {
		return nil, err
	}
	if err := cs.RunMigrations(ctx); err != nil {
		return nil, err
	}
	if err := cs.VerifySchemaVersion(ctx); err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "database migrations applied",
		logfmt.Component("di"),
		"driver", cfg.Database.Driver,
	)

	if err := cs.SyncQuotaLimits(ctx, cfg.Backends); err != nil {
		return nil, err
	}
	return cs, nil
}

// ProvideDatabaseBreaker constructs the shared *breaker.CircuitBreaker every
// driver-level SQL statement forwards calls through.
func ProvideDatabaseBreaker(i do.Injector) (*breaker.CircuitBreaker, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	return store.NewDatabaseBreaker(cfg.CircuitBreaker), nil
}

// openStore dispatches store construction to the configured driver. The
// shared *breaker.CircuitBreaker is threaded through so every SQL
// statement (pool-bound or tx-bound) gets PreCheck/PostCheck protection
// at the driver chokepoint.
func openStore(ctx context.Context, dbCfg *config.DatabaseConfig, cb *breaker.CircuitBreaker) (core.MetadataStore, error) {
	switch dbCfg.Driver {
	case "postgres":
		return postgres.NewStore(ctx, dbCfg, cb)
	case "sqlite":
		return sqlitestore.NewStore(ctx, dbCfg, cb)
	default:
		return nil, fmt.Errorf("unsupported database driver: %q", dbCfg.Driver)
	}
}

// ProvideInstanceID resolves a stable per-process identifier used as the
// claimed_by stamp on cleanup_queue rows. The identifier is generated once
// at first invoke and reused everywhere the value is needed.
func ProvideInstanceID(_ do.Injector) (instanceid.ID, error) {
	return instanceid.New()
}
