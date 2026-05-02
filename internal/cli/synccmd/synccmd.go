// -------------------------------------------------------------------------------
// Sync CLI - Import Pre-Existing Bucket Objects
//
// Author: Alex Freidah
//
// Scans a backend S3 bucket via ListObjectsV2 and imports discovered objects
// into the proxy's metadata database. Objects already tracked for the backend
// are skipped. Useful when bringing an existing bucket under proxy management.
// -------------------------------------------------------------------------------

package synccmd

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"github.com/afreidah/s3-orchestrator/internal/store/postgres"
	sqlitestore "github.com/afreidah/s3-orchestrator/internal/store/sqlite"
)

// Options holds the parsed CLI flags for `s3-orchestrator sync`.
type Options struct {
	ConfigPath  string
	BackendName string
	BucketName  string
	Prefix      string
	DryRun      bool
}

// Run is the CLI entry point. It parses the sync flags, opens the database,
// and walks the backend, returning the process exit code.
func Run(args []string, stderr io.Writer) int { // codecov:ignore -- CLI entry point
	opts, ok := parseFlags(args, stderr)
	if !ok {
		return 1
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg, backendCfg, exit := loadConfig(opts.ConfigPath, opts.BackendName)
	if exit != 0 {
		return exit
	}

	ctx := context.Background()
	metaDB, adminDB, exit := initStore(ctx, cfg)
	if exit != 0 {
		return exit
	}
	defer adminDB.Close()

	s3b, err := backend.NewS3Backend(backendCfg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to initialize backend", "backend", backendCfg.Name, "error", err)
		return 1
	}

	if err := runImport(ctx, s3b, metaDB, backendCfg, opts); err != nil {
		slog.ErrorContext(ctx, "sync failed", "error", err)
		return 1
	}
	return 0
}

// parseFlags parses the CLI flags for the sync subcommand and enforces the
// required ones. Returns ok=false when a required flag is missing.
func parseFlags(args []string, stderr io.Writer) (*Options, bool) {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	opts := &Options{}
	fs.StringVar(&opts.ConfigPath, "config", "config.yaml", "Path to configuration file")
	fs.StringVar(&opts.BackendName, "backend", "", "Backend name to sync (required)")
	fs.StringVar(&opts.BucketName, "bucket", "", "Virtual bucket name to prefix imported keys with (required)")
	fs.StringVar(&opts.Prefix, "prefix", "", "Only sync objects with this key prefix")
	fs.BoolVar(&opts.DryRun, "dry-run", false, "Preview what would be imported without writing")
	_ = fs.Parse(args)

	if opts.BackendName == "" {
		fmt.Fprintln(stderr, "error: --backend is required")
		fs.Usage()
		return nil, false
	}
	if opts.BucketName == "" {
		fmt.Fprintln(stderr, "error: --bucket is required")
		fs.Usage()
		return nil, false
	}
	return opts, true
}

// loadConfig reads config.yaml and resolves the target backend by name.
// Returns a non-zero exit code when either step fails.
func loadConfig(path, backendName string) (*config.Config, *config.BackendConfig, int) {
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

// initStore opens the metadata store, applies migrations, and syncs quota
// limits. Returns non-zero exit on any failure. sync only writes new object
// rows, so it asks for the narrow ObjectStore plus the admin handle required
// by RunMigrations / SyncQuotaLimits.
func initStore(ctx context.Context, cfg *config.Config) (store.ObjectStore, store.AdminStore, int) {
	var (
		objects store.ObjectStore
		adminDB store.AdminStore
		err     error
	)
	switch cfg.Database.Driver {
	case "postgres":
		s, openErr := postgres.NewStore(ctx, &cfg.Database)
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

// runImport walks the backend, importing each page into the metadata store.
// Accumulates and logs totals per page.
func runImport(ctx context.Context, s3b *backend.S3Backend, metaDB store.ObjectStore, backendCfg *config.BackendConfig, opts *Options) error {
	mode := "sync"
	if opts.DryRun {
		mode = "dry-run"
	}
	slog.InfoContext(ctx, "starting sync",
		"backend", backendCfg.Name,
		"virtual_bucket", opts.BucketName,
		"backend_bucket", backendCfg.Bucket,
		"prefix", opts.Prefix,
		"mode", mode,
	)

	var totalImported, totalSkipped int
	var totalBytes int64
	pageNum := 0

	err := s3b.ListObjects(ctx, opts.Prefix, func(objects []backend.ListedObject) error {
		pageNum++
		imported, skipped, bytes, err := importPage(ctx, metaDB, objects, backendCfg.Name, opts)
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

// importPage imports one page of backend objects into the metadata store
// (or logs them under dry-run), returning per-page counters.
func importPage(ctx context.Context, metaDB store.ObjectStore, objects []backend.ListedObject, backendName string, opts *Options) (imported, skipped int, bytes int64, err error) {
	for _, obj := range objects {
		prefixedKey := opts.BucketName + "/" + obj.Key
		if opts.DryRun {
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