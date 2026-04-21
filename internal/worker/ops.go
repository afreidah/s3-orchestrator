// -------------------------------------------------------------------------------
// Worker Dependency Contracts — Role-Based Interfaces
//
// Author: Alex Freidah
//
// Defines focused role interfaces that background workers compose to declare
// their exact dependencies. The proxy.BackendManager implements all of them.
// Each worker receives its own narrow store interface rather than accessing
// a shared MetadataStore god object.
// -------------------------------------------------------------------------------

// Package worker contains background services that maintain storage health:
// rebalancing, replication, over-replication cleanup, orphan cleanup queue
// processing, and reconciliation. Each worker receives composed interfaces
// for its dependencies rather than embedding shared infrastructure.
package worker

//go:generate mockgen -destination=mock_ops_test.go -package=worker github.com/afreidah/s3-orchestrator/internal/worker Ops,CleanupOps,ScrubberOps,BackendSyncer

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/store"
)

// -------------------------------------------------------------------------
// NARROW STORE INTERFACES (per-worker)
// -------------------------------------------------------------------------

// RebalancerStore defines the store operations the Rebalancer needs.
type RebalancerStore interface {
	store.ObjectStore
	store.QuotaStore
}

// ReplicatorStore defines the store operations the Replicator needs.
type ReplicatorStore interface {
	store.ObjectStore
	store.ReplicationStore
	store.QuotaStore
}

// OverReplicationStore defines the store operations the OverReplicationCleaner needs.
type OverReplicationStore interface {
	store.ReplicationStore
	store.QuotaStore
}

// CleanupWorkerStore defines the store operations the CleanupWorker needs.
type CleanupWorkerStore interface {
	store.CleanupStore
}

// ScrubberStore defines the store operations the Scrubber needs.
type ScrubberStore interface {
	store.IntegrityStore
}

// -------------------------------------------------------------------------
// ROLE INTERFACES (non-store infrastructure)
// -------------------------------------------------------------------------

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

// UsageAccess provides usage tracking.
type UsageAccess interface {
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
	UsageAccess
}

// CleanupOps is the dependency contract for CleanupWorker. It omits
// BackendAccess because cleanup operates on specific named backends.
type CleanupOps interface {
	AdmissionControl
	DataMover
	UsageAccess
}

// ScrubberOps is the dependency contract for Scrubber. It omits
// AdmissionControl and BackendAccess because integrity checks are
// best-effort background work.
type ScrubberOps interface {
	DataMover
	UsageAccess
}
