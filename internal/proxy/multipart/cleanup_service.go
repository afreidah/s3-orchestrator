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
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// DefaultMultipartCleanupTick is how often the multipart cleanup runs.
const DefaultMultipartCleanupTick = 1 * time.Hour

// NewCleanupService constructs the multipart-cleanup background
// service backed by mgr.CleanupStaleMultipartUploads, abandoning uploads
// older than staleTimeout.
func NewCleanupService(mgr *Manager, locker tickrunner.AdvisoryLocker, staleTimeout time.Duration) lifecycle.Runner {
	return tickrunner.New(tickrunner.Config{
		Locker:   locker,
		Interval: DefaultMultipartCleanupTick,
		LockID:   core.LockMultipartCleanup,
		Name:     string(audit.WorkloadMultipartCleanup),
		Work: func(ctx context.Context) error {
			mgr.CleanupStaleMultipartUploads(ctx, staleTimeout)
			return nil
		},
	})
}
