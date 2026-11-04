// -------------------------------------------------------------------------------
// Store Engine - The Union of the Store Roles
//
// Author: Alex Freidah
//
// Names everything one metadata store engine implements, for the few sites that
// hold a whole engine: the opener, the composition root and the generated mock.
// It sits apart from internal/store/core, which every consumer imports, because
// the union is wiring rather than domain, and depguard keeps feature packages
// from importing it.
// -------------------------------------------------------------------------------

// Package engine names the union of the metadata store roles.
package engine

import "github.com/afreidah/s3-orchestrator/internal/store/core"

// Store is the union of every store role. Consumers depend on the individual
// roles in internal/store/core instead.
type Store interface {
	core.ObjectStore
	core.QuotaStore
	core.MultipartStore
	core.ReplicationStore
	core.CleanupStore
	core.PendingStore
	core.IntegrityStore
	core.ExpiredObjectsLister
	core.BackendStateStore
	core.UsageFlusher
	core.UsageReader
	core.AdvisoryLocker
	core.DashboardStore
	core.LifecycleAdmin
	core.NotificationOutbox
	core.TagStore
	core.ProvisioningStore
	core.Pinger
}
