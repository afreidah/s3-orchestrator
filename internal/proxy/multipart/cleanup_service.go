// -------------------------------------------------------------------------------
// Multipart Cleanup - Background Service Constructor
//
// Author: Alex Freidah
//
// Wraps *Manager.CleanupStaleMultipartUploads in a lifecycle.Runner
// backed by the shared advisory-locked ticker primitive. Lives in the
// multipart package (next to the worker that owns the work) rather
// than internal/di.
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

// NewCleanupService constructs the multipart-cleanup background
// service backed by mgr.CleanupStaleMultipartUploads.
func NewCleanupService(mgr *Manager, locker tickrunner.AdvisoryLocker, staleTimeout time.Duration) lifecycle.Runner {
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
			mgr.CleanupStaleMultipartUploads(ctx, staleTimeout)
			return nil
		},
	})
}
