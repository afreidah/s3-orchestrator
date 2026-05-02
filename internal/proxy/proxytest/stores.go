// -------------------------------------------------------------------------------
// proxytest - Cross-Package Test Helpers for the Proxy Package
//
// Author: Alex Freidah
//
// Holds helpers that build proxy types from test fixtures. Lives in a
// dedicated subpackage so the production package's stores.go stays free
// of test-only API. Imported only from *_test.go files.
// -------------------------------------------------------------------------------

// Package proxytest provides cross-package test helpers for the proxy
// package. Importing it from production code is not supported.
package proxytest

import (
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// AllRoles is the structural requirement StoresFromMock imposes on its
// argument: any value that satisfies every narrow role interface.
type AllRoles interface {
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

// StoresFromMock returns a proxy.Stores bag where every field points at
// the same value. Test-only: pass a single mock that satisfies all role
// interfaces (e.g. testutil.MockStore) and receive a fully-populated
// Stores ready to drop into BackendManagerConfig.
func StoresFromMock(m AllRoles) proxy.Stores {
	return proxy.Stores{
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
