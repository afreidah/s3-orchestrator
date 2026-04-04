// -------------------------------------------------------------------------------
// Worker Dependency Contracts — Role-Based Interfaces
//
// Author: Alex Freidah
//
// Defines focused role interfaces that background workers compose to declare
// their exact dependencies. The proxy.BackendManager implements all of them.
// -------------------------------------------------------------------------------

// Package worker contains background services that maintain storage health:
// rebalancing, replication, over-replication cleanup, orphan cleanup queue
// processing, and reconciliation. Each worker receives composed interfaces
// for its dependencies rather than embedding shared infrastructure.
package worker

//go:generate mockgen -destination=mock_ops_test.go -package=worker github.com/afreidah/s3-orchestrator/internal/worker Ops,CleanupDeps,ScrubberDeps,BackendSyncer

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/store"
)

// -------------------------------------------------------------------------
// ROLE INTERFACES
// -------------------------------------------------------------------------

// StoreAccess provides metadata store and usage tracking.
type StoreAccess interface {
	Store() store.MetadataStore
	Usage() *counter.UsageTracker
}

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

// -------------------------------------------------------------------------
// COMPOSED DEPENDENCY CONTRACTS
// -------------------------------------------------------------------------

// Ops combines all worker dependency interfaces for workers that need full
// backend fleet access, store access, admission control, and data movement.
// Used by Replicator, Rebalancer, and OverReplicationCleaner.
type Ops interface {
	StoreAccess
	BackendAccess
	AdmissionControl
	DataMover
}

// CleanupDeps is the dependency contract for CleanupWorker. It omits
// BackendAccess because cleanup operates on specific named backends
// via DataMover.GetBackend, not fleet-wide discovery.
type CleanupDeps interface {
	StoreAccess
	AdmissionControl
	DataMover
}

// ScrubberDeps is the dependency contract for Scrubber. It omits
// AdmissionControl and BackendAccess because integrity checks are
// best-effort background work that reads from specific backends.
type ScrubberDeps interface {
	StoreAccess
	DataMover
}
