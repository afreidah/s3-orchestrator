// -------------------------------------------------------------------------------
// Sync Subcommand - Import Pre-Existing Bucket Objects
//
// Author: Alex Freidah
//
// Scans a backend S3 bucket via ListObjectsV2 and imports discovered objects
// into the proxy's metadata database. Objects already tracked for the backend
// are skipped. Useful when bringing an existing bucket under proxy management.
// -------------------------------------------------------------------------------

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store"
	sqlitestore "github.com/afreidah/s3-orchestrator/internal/store/sqlite"
)

// syncOpts holds the parsed CLI flags for `s3-orchestrator sync`.
type syncOpts struct {
	configPath  string
	backendName string
	bucketName  string
	prefix      string
	dryRun      bool
}

func runSync() {
	opts, ok := parseSyncFlags()
	if !ok {
		os.Exit(1)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg, backendCfg, exit := loadSyncConfig(opts.configPath, opts.backendName)
	if exit != 0 {
		os.Exit(exit)
	}

	ctx := context.Background()
	metaDB, adminDB, exit := initSyncStore(ctx, cfg)
	if exit != 0 {
		os.Exit(exit)
	}
	defer adminDB.Close()

	s3b, err := backend.NewS3Backend(backendCfg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to initialize backend", "backend", backendCfg.Name, "error", err)
		os.Exit(1)
	}

	if err := runSyncImport(ctx, s3b, metaDB, backendCfg, opts); err != nil {
		slog.ErrorContext(ctx, "sync failed", "error", err)
		os.Exit(1)
	}
}

// parseSyncFlags parses the CLI flags for the sync subcommand and enforces
// the required ones. Returns ok=false when a required flag is missing.
func parseSyncFlags() (*syncOpts, bool) {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	opts := &syncOpts{}
	fs.StringVar(&opts.configPath, "config", "config.yaml", "Path to configuration file")
	fs.StringVar(&opts.backendName, "backend", "", "Backend name to sync (required)")
	fs.StringVar(&opts.bucketName, "bucket", "", "Virtual bucket name to prefix imported keys with (required)")
	fs.StringVar(&opts.prefix, "prefix", "", "Only sync objects with this key prefix")
	fs.BoolVar(&opts.dryRun, "dry-run", false, "Preview what would be imported without writing")
	_ = fs.Parse(os.Args[1:])

	if opts.backendName == "" {
		fmt.Fprintln(os.Stderr, "error: --backend is required")
		fs.Usage()
		return nil, false
	}
	if opts.bucketName == "" {
		fmt.Fprintln(os.Stderr, "error: --bucket is required")
		fs.Usage()
		return nil, false
	}
	return opts, true
}

// loadSyncConfig reads config.yaml and resolves the target backend by name.
// Returns a non-zero exit code when either step fails.
func loadSyncConfig(path, backendName string) (*config.Config, *config.BackendConfig, int) {
	cfg, err := config.LoadConfig(path)
	if err != nil {
		slog.Error("Failed to load config", "error", err) //nolint:sloglint // no context before DB init
		return nil, nil, 1
	}
	for i := range cfg.Backends {
		if cfg.Backends[i].Name == backendName {
			return cfg, &cfg.Backends[i], 0
		}
	}
	slog.Error("Backend not found in config", "backend", backendName) //nolint:sloglint // no context before DB init
	return nil, nil, 1
}

// initSyncStore opens the metadata store, applies migrations, and syncs
// quota limits. Returns non-zero exit on any failure. sync only writes
// new object rows, so it asks for the narrow ObjectStore plus the admin
// handle required by RunMigrations / SyncQuotaLimits.
func initSyncStore(ctx context.Context, cfg *config.Config) (store.ObjectStore, store.AdminStore, int) {
	var (
		objects store.ObjectStore
		adminDB store.AdminStore
		err     error
	)
	switch cfg.Database.Driver {
	case "postgres":
		s, openErr := store.NewStore(ctx, &cfg.Database)
		if openErr != nil {
			err = openErr
		} else {
			objects, adminDB = s, s
		}
	case "sqlite":
		s, openErr := sqlitestore.NewStore(ctx, &cfg.Database)
		if openErr != nil {
			err = openErr
		} else {
			objects, adminDB = s, s
		}
	default:
		err = fmt.Errorf("unsupported database driver: %q", cfg.Database.Driver)
	}
	if err != nil {
		slog.ErrorContext(ctx, "failed to connect to database", "error", err)
		return nil, nil, 1
	}
	if err := adminDB.RunMigrations(ctx); err != nil {
		slog.ErrorContext(ctx, "failed to run migrations", "error", err)
		return nil, nil, 1
	}
	if err := adminDB.SyncQuotaLimits(ctx, cfg.Backends); err != nil {
		slog.ErrorContext(ctx, "failed to sync quota limits", "error", err)
		return nil, nil, 1
	}
	return objects, adminDB, 0
}

// runSyncImport walks the backend, importing each page into the metadata
// store. Accumulates + logs totals per page.
func runSyncImport(ctx context.Context, s3b *backend.S3Backend, metaDB store.ObjectStore, backendCfg *config.BackendConfig, opts *syncOpts) error {
	mode := "sync"
	if opts.dryRun {
		mode = "dry-run"
	}
	slog.InfoContext(ctx, "starting sync",
		"backend", backendCfg.Name,
		"virtual_bucket", opts.bucketName,
		"backend_bucket", backendCfg.Bucket,
		"prefix", opts.prefix,
		"mode", mode,
	)

	var totalImported, totalSkipped int
	var totalBytes int64
	pageNum := 0

	err := s3b.ListObjects(ctx, opts.prefix, func(objects []backend.ListedObject) error {
		pageNum++
		imported, skipped, bytes, err := importSyncPage(ctx, metaDB, objects, backendCfg.Name, opts)
		if err != nil {
			return err
		}
		totalImported += imported
		totalSkipped += skipped
		totalBytes += bytes
		slog.InfoContext(ctx, "synced page",
			"page", pageNum, "imported", imported, "skipped", skipped,
			"total_imported", totalImported, "total_skipped", totalSkipped,
		)
		return nil
	})
	if err != nil {
		return err
	}

	slog.InfoContext(ctx, "sync complete",
		"backend", backendCfg.Name,
		"imported", totalImported,
		"skipped", totalSkipped,
		"bytes_imported", totalBytes,
		"mode", mode,
	)
	return nil
}

// importSyncPage imports one page of backend objects into the metadata
// store (or logs them under dry-run), returning per-page counters.
func importSyncPage(ctx context.Context, metaDB store.ObjectStore, objects []backend.ListedObject, backendName string, opts *syncOpts) (imported, skipped int, bytes int64, err error) {
	for _, obj := range objects {
		prefixedKey := opts.bucketName + "/" + obj.Key
		if opts.dryRun {
			slog.InfoContext(ctx, "would import", "key", prefixedKey, "size", obj.SizeBytes)
			imported++
			bytes += obj.SizeBytes
			continue
		}
		ok, err := metaDB.ImportObject(ctx, prefixedKey, backendName, obj.SizeBytes)
		if err != nil {
			return imported, skipped, bytes, fmt.Errorf("failed to import %s: %w", obj.Key, err)
		}
		if ok {
			imported++
			bytes += obj.SizeBytes
		} else {
			skipped++
		}
	}
	return imported, skipped, bytes, nil
}
