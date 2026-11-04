// -------------------------------------------------------------------------------
// Store Opening
//
// Author: Alex Freidah
//
// Opens the configured metadata store and brings it up to what this build
// expects. The server and the sync command both start here, so a driver or a
// boot step added once reaches both.
// -------------------------------------------------------------------------------

package store

import (
	"context"
	"fmt"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/engine"
	"github.com/afreidah/s3-orchestrator/internal/store/postgres"
	sqlitestore "github.com/afreidah/s3-orchestrator/internal/store/sqlite"
)

// Open dispatches store construction to the configured driver. cb gives every
// SQL statement PreCheck/PostCheck protection at the driver chokepoint, and
// instanceID claims the cleanup rows the engine's transactions queue.
func Open(ctx context.Context, dbCfg *config.DatabaseConfig, cb *breaker.CircuitBreaker, instanceID string) (engine.Store, error) {
	switch dbCfg.Driver {
	case "postgres":
		return postgres.NewStore(ctx, dbCfg, cb, instanceID)
	case "sqlite":
		return sqlitestore.NewStore(ctx, dbCfg, cb, instanceID)
	default:
		return nil, fmt.Errorf("unsupported database driver: %q", dbCfg.Driver)
	}
}

// Prepare applies migrations, checks the schema is the one this build expects,
// and syncs the configured quota limits, in the order a boot needs them.
func Prepare(ctx context.Context, s core.LifecycleAdmin, backends []config.BackendConfig) error {
	if err := s.RunMigrations(ctx); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	if err := s.VerifySchemaVersion(ctx); err != nil {
		return fmt.Errorf("verify schema version: %w", err)
	}
	if err := s.SyncQuotaLimits(ctx, backends); err != nil {
		return fmt.Errorf("sync quota limits: %w", err)
	}
	return nil
}
