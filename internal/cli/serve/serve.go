// -------------------------------------------------------------------------------
// Serve - s3-orchestrator serve subcommand
//
// Author: Alex Freidah
//
// CLI composition root. Loads configuration from disk and hands control to
// the runtime package which owns daemon assembly and lifecycle. Keeping
// this file thin makes the surface the cmd binary calls (Run) easy to
// understand and test - everything substantive lives one package deeper.
// -------------------------------------------------------------------------------

// Package serve implements the `s3-orchestrator serve` subcommand. It is
// a thin composition root: load the configuration file, construct a
// runtime.Runtime, and run it. All daemon assembly lives in
// internal/runtime.
package serve

import (
	"context"
	"fmt"
	"io"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/runtime"
)

// Run is the daemon entry point. Loads config, hands control to the
// runtime package, and blocks until ctx is cancelled (SIGINT/SIGTERM)
// or the runtime returns an error. Errors are returned to the caller
// instead of calling os.Exit so this function is unit-testable.
func Run(ctx context.Context, configPath, mode string, stdout io.Writer) error {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	m, err := config.ParseMode(mode)
	if err != nil {
		return err
	}

	rt, err := runtime.New(runtime.Options{
		ConfigPath: configPath,
		Mode:       m,
		Stdout:     stdout,
	}, cfg)
	if err != nil {
		return err
	}

	return rt.Run(ctx)
}
