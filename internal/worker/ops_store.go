// -------------------------------------------------------------------------------
// Worker Dependency Contracts  -  Per-Worker Store Roles
//
// Author: Alex Freidah
//
// Each background worker composes the narrow core store roles it actually
// uses into one named interface. The proxy package's wiring builds adapter
// values that satisfy these contracts; workers depend only on the role
// they ask for and never see a full MetadataStore-shaped god type.
// -------------------------------------------------------------------------------

// Package worker contains background services that maintain storage
// health: rebalancing, replication, over-replication cleanup, orphan
// cleanup queue processing, and reconciliation. Each worker receives
// composed interfaces for its dependencies rather than embedding shared
// infrastructure.
package worker

import "github.com/afreidah/s3-orchestrator/internal/store/core"

// RebalancerStore defines the store operations the Rebalancer needs.
type RebalancerStore interface {
	core.ObjectStore
	core.QuotaStore
}

// ReplicatorStore defines the store operations the Replicator needs.
type ReplicatorStore interface {
	core.ObjectStore
	core.ReplicationStore
	core.QuotaStore
}

// OverReplicationStore defines the store operations the
// OverReplicationCleaner needs.
type OverReplicationStore interface {
	core.ReplicationStore
	core.QuotaStore
}

// CleanupWorkerStore defines the store operations the CleanupWorker needs.
type CleanupWorkerStore interface {
	core.CleanupStore
}

// ScrubberStore defines the store operations the Scrubber needs.
type ScrubberStore interface {
	core.IntegrityStore
}
