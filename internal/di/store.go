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

// metadataStore is the union of every store role, and exists only so the
// composition root can carry one opened engine from openStore to the
// do.MustAs role aliases in injector.go.
//
// Unexported on purpose. The union is not a domain fact: nothing in the
// persistence domain requires one object to implement all seventeen roles,
// and a design that split persistence across several objects would satisfy
// every role interface without violating anything. What it actually encodes
// is that this package holds one opened value before splitting it, which is
// wiring. Keeping it unnameable outside di is what stops it being taken as a
// dependency, since consumers cannot depend on a type they cannot write down.
//
// Each engine also carries per-role compile-time assertions, so a driver that
// drops a method names the role rather than the union.
type metadataStore interface {
	core.ObjectStore
	core.QuotaStore
	core.MultipartStore
	core.ReplicationStore
	core.CleanupStore
	core.PendingStore
	core.IntegrityStore
	core.ExpiredObjectsLister
	core.BackendLifecycleStore
	core.UsageFlusher
	core.AdvisoryLocker
	core.DashboardStore
	core.LifecycleAdmin
	core.EncryptionAdmin
	core.CompressionAdmin
	core.NotificationOutbox
	core.TagStore
}

// provideMetadataStore opens the configured driver, runs migrations, syncs
// quota limits, and returns the opened engine for the role aliases to split.
// CB protection lives inside each driver's DBTX/DB chokepoint, so this
// provider does no wrapping of its own.
func provideMetadataStore(i do.Injector) (metadataStore, error) {
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
func openStore(ctx context.Context, dbCfg *config.DatabaseConfig, cb *breaker.CircuitBreaker) (metadataStore, error) {
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
