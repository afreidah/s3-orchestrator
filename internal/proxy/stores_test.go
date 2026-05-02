// -------------------------------------------------------------------------------
// In-Package Test Helpers
//
// Author: Alex Freidah
//
// Mirror of testStoresFromMock for tests inside the proxy package.
// proxy's own tests can't import proxy/proxytest without an import cycle,
// so this file declares a local copy guarded by the _test.go build tag.
// External callers must use testStoresFromMock instead.
// -------------------------------------------------------------------------------

package proxy

import "github.com/afreidah/s3-orchestrator/internal/store/core"

// allRoles is the structural shape testStoresFromMock requires of its
// argument — any value that satisfies every narrow role interface.
type allRoles interface {
	core.ObjectStore
	core.QuotaStore
	core.MultipartStore
	core.ReplicationStore
	core.CleanupStore
	core.PendingStore
	core.IntegrityStore
	core.ExpiredObjectsLister
	core.BackendLifecycleStore
	core.DashboardStore
	core.UsageFlusher
	core.AdvisoryLocker
}

// testStoresFromMock returns a Stores bag where every field points at the
// same mock value. Used by tests inside the proxy package; tests in other
// packages use testStoresFromMock instead.
func testStoresFromMock(m allRoles) Stores {
	return Stores{
		Object:           m,
		Quota:            m,
		Multipart:        m,
		Replication:      m,
		Cleanup:          m,
		Pending:          m,
		Integrity:        m,
		Lifecycle:        m,
		BackendLifecycle: m,
		Dashboard:        m,
		Usage:            m,
		Lock:             m,
	}
}
