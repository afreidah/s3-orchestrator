// -------------------------------------------------------------------------------
// Multipart Cleanup - Background Service Constructor
//
// Author: Alex Freidah
//
// Wraps *Manager.CleanupStaleMultipartUploads in a lifecycle.Runner
// backed by the shared advisory-locked ticker primitive. Lives in the
// multipart package (next to the worker that owns the work) rather
// than internal/di (#925).
// -------------------------------------------------------------------------------

package multipart

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle/tickrunner"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// Default cleanup parameters used when the config does not override them.
const (
	DefaultMultipartStaleTimeout = 24 * time.Hour
	DefaultMultipartCleanupTick  = 1 * time.Hour
)

// StaleCleaner is the consumer-declared surface NewCleanupService
// needs from *Manager. Declared here so this constructor does not
// pull the wider Manager surface into its dependency graph.
type StaleCleaner interface {
	CleanupStaleMultipartUploads(ctx context.Context, olderThan time.Duration)
}

// NewCleanupService constructs the multipart-cleanup background
// service. The cleaner is typically *multipart.Manager; the narrow
// interface keeps this constructor decoupled from the rest of
// *proxy.BackendManager.
func NewCleanupService(cleaner StaleCleaner, locker tickrunner.AdvisoryLocker, staleTimeout time.Duration) lifecycle.Runner {
	if staleTimeout <= 0 {
		staleTimeout = DefaultMultipartStaleTimeout
	}
	const slug = "multipart_cleanup"
	return tickrunner.New(tickrunner.Config{
		Locker:   locker,
		Interval: DefaultMultipartCleanupTick,
		LockID:   core.LockMultipartCleanup,
		Name:     slug,
		Log:      tickrunner.ComponentLogger(slug),
		Work: func(ctx context.Context) error {
			cleaner.CleanupStaleMultipartUploads(ctx, staleTimeout)
			return nil
		},
	})
}
