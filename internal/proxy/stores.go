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

import (
	"github.com/afreidah/s3-orchestrator/internal/store"
)

// Stores groups the narrow store roles injected into BackendManager.
// Each field is populated at DI wiring time with a CB-wrapped view of the
// concrete store. Consumers read the field that matches their role.
type Stores struct {
	Object           store.ObjectStore
	Quota            store.QuotaStore
	Multipart        store.MultipartStore
	Replication      store.ReplicationStore
	Cleanup          store.CleanupStore
	Integrity        store.IntegrityStore
	Lifecycle        store.ExpiredObjectsLister
	BackendLifecycle store.BackendLifecycleStore
	Dashboard        store.DashboardStore
	Usage            store.UsageFlusher
	Lock             store.AdvisoryLocker
}

// rebalancerStore satisfies worker.RebalancerStore by embedding the two
// narrow roles the rebalancer actually touches.
type rebalancerStore struct {
	store.ObjectStore
	store.QuotaStore
}

// replicatorStore satisfies worker.ReplicatorStore.
type replicatorStore struct {
	store.ObjectStore
	store.ReplicationStore
	store.QuotaStore
}

// overReplicationStore satisfies worker.OverReplicationStore.
type overReplicationStore struct {
	store.ReplicationStore
	store.QuotaStore
}

// StoresFromMock returns a Stores bag where every field points at the same
// value. Intended for tests: pass a single mock that satisfies all role
// interfaces (e.g. testutil.MockStore) and receive a fully-populated Stores
// ready to drop into BackendManagerConfig.
func StoresFromMock(m allRoles) Stores {
	return Stores{
		Object:           m,
		Quota:            m,
		Multipart:        m,
		Replication:      m,
		Cleanup:          m,
		Integrity:        m,
		Lifecycle:        m,
		BackendLifecycle: m,
		Dashboard:        m,
		Usage:            m,
		Lock:             m,
	}
}

// allRoles is the structural requirement StoresFromMock imposes on its
// argument — any value that satisfies every narrow role interface can be
// spread across a Stores bag. Declared inline rather than exported because
// only tests need this composite shape.
type allRoles interface {
	store.ObjectStore
	store.QuotaStore
	store.MultipartStore
	store.ReplicationStore
	store.CleanupStore
	store.IntegrityStore
	store.ExpiredObjectsLister
	store.BackendLifecycleStore
	store.DashboardStore
	store.UsageFlusher
	store.AdvisoryLocker
}