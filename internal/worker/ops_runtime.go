// -------------------------------------------------------------------------------
// Worker Dependency Contracts  -  Runtime Infrastructure Roles
//
// Author: Alex Freidah
//
// The non-store infrastructure each background worker needs from the
// proxy layer: backend fleet discovery, admission gating, data-movement
// primitives, and usage accounting. Composed types at the bottom of the
// file pin which subset each worker takes.
// -------------------------------------------------------------------------------

package worker

//go:generate mockgen -destination=mock_ops_test.go -package=worker github.com/afreidah/s3-orchestrator/internal/worker Ops,CleanupOps,ScrubberOps,BackendSyncer

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/counter"
)

// BackendAccess provides backend fleet discovery and drain-awareness.
type BackendAccess interface {
	Backends() map[string]backend.ObjectBackend
	BackendOrder() []string
	IsDraining(name string) bool
	ExcludeDraining(eligible []string) []string
	SelectReplicaTarget(ctx context.Context, size int64, exclusion map[string]bool) (string, error)
}

// AdmissionControl gates concurrent access to backends.
type AdmissionControl interface {
	AcquireAdmission(ctx context.Context) bool
	ReleaseAdmission()
}

// DataMover provides object data movement primitives.
type DataMover interface {
	GetBackend(name string) (backend.ObjectBackend, error)
	WithTimeout(ctx context.Context) (context.Context, context.CancelFunc)
	StreamCopy(ctx context.Context, src, dst backend.ObjectBackend, key string) error
	DeleteWithTimeout(ctx context.Context, be backend.ObjectBackend, key string) error
	DeleteOrEnqueue(ctx context.Context, be backend.ObjectBackend, backendName, key, reason string, sizeBytes int64)
}

// UsageAccessor provides usage tracking.
type UsageAccessor interface {
	Usage() *counter.UsageTracker
}

// -------------------------------------------------------------------------
// COMPOSED DEPENDENCY CONTRACTS
// -------------------------------------------------------------------------

// Ops combines fleet access, admission control, data movement, and usage
// tracking. Used by Rebalancer, Replicator, and OverReplicationCleaner.
type Ops interface {
	BackendAccess
	AdmissionControl
	DataMover
	UsageAccessor
}

// CleanupOps is the dependency contract for CleanupWorker. It omits
// BackendAccess because cleanup operates on specific named backends.
type CleanupOps interface {
	AdmissionControl
	DataMover
	UsageAccessor
}

// ScrubberOps is the dependency contract for Scrubber. It omits
// AdmissionControl and BackendAccess because integrity checks are
// best-effort background work.
type ScrubberOps interface {
	DataMover
	UsageAccessor
}
