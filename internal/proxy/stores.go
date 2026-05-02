// -------------------------------------------------------------------------------
// Stores — per-role store dependency bag
//
// Author: Alex Freidah
//
// Stores is a plain container for the narrow role interfaces the proxy layer
// and its workers need. It is NOT an interface — nothing can claim to
// "implement Stores". Each field names the role explicitly, so adding a new
// method to the storage layer forces a decision about which role owns it.
//
// Per-worker dependency types (rebalancerStore, replicatorStore, ...) are
// small adapter structs that embed the specific narrow interfaces each
// worker asks for; they exist only so NewBackendManager can hand workers a
// value that satisfies their compound dependency contracts in
// internal/worker/ops.go.
// -------------------------------------------------------------------------------

package proxy

import "github.com/afreidah/s3-orchestrator/internal/store/core"

// Stores groups the narrow store roles injected into BackendManager.
// Each field is populated at DI wiring time with a CB-wrapped view of the
// concrete store. Consumers read the field that matches their role.
type Stores struct {
	Object           core.ObjectStore
	Quota            core.QuotaStore
	Multipart        core.MultipartStore
	Replication      core.ReplicationStore
	Cleanup          core.CleanupStore
	Pending          core.PendingStore
	Integrity        core.IntegrityStore
	Lifecycle        core.ExpiredObjectsLister
	BackendLifecycle core.BackendLifecycleStore
	Dashboard        core.DashboardStore
	Usage            core.UsageFlusher
	Lock             core.AdvisoryLocker
}

// rebalancerStore satisfies worker.RebalancerStore by embedding the two
// narrow roles the rebalancer actually touches.
type rebalancerStore struct {
	core.ObjectStore
	core.QuotaStore
}

// replicatorStore satisfies worker.ReplicatorStore.
type replicatorStore struct {
	core.ObjectStore
	core.ReplicationStore
	core.QuotaStore
}

// overReplicationStore satisfies worker.OverReplicationStore.
type overReplicationStore struct {
	core.ReplicationStore
	core.QuotaStore
}

