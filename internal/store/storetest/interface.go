// Package storetest hosts the mockgen-generated MockMetadataStore used
// across the test suite as a single drop-in replacement for the
// hand-written wide store mocks that previously lived in internal/proxy,
// internal/store, internal/testutil, and internal/di.
package storetest

import "github.com/afreidah/s3-orchestrator/internal/store/core"

//go:generate mockgen -destination=mocks.go -package=storetest github.com/afreidah/s3-orchestrator/internal/store/storetest MetadataStore

// Per-role mocks let a test that exercises one capability stub that
// capability alone, instead of standing up the 79-method union and
// silencing the rest with Permissive. The list carries only the roles a
// test actually mocks today - add a name when the first consumer appears
// rather than generating mocks nothing calls.
//go:generate mockgen -destination=role_mocks.go -package=storetest github.com/afreidah/s3-orchestrator/internal/store/core ObjectStore,QuotaStore,CleanupStore,ExpiredObjectsLister,BackendLifecycleStore,DashboardStore,LifecycleAdmin

// MetadataStore is the union of every narrow store role interface. It
// exists only as a mockgen target so a single generated MockMetadataStore
// can stand in wherever a fully-populated proxy.ManagerStores or a
// concreteStore-shaped DI value is needed. Production code never depends
// on this composite - production callers consume the narrow roles
// declared in internal/store/core directly.
//
// QuotaStore.GetQuotaStats and DashboardStore.GetQuotaStats share a
// signature; embedded interfaces flatten to a single method on the
// outer interface, which is why this composite must be declared rather
// than synthesised by struct embedding of per-role mocks.
type MetadataStore interface {
	core.ObjectStore
	core.QuotaStore
	core.MultipartStore
	core.ReplicationStore
	core.CleanupStore
	core.PendingStore
	core.IntegrityStore
	core.ExpiredObjectsLister
	core.BackendLifecycleStore
	core.UsageFlusher
	core.AdvisoryLocker
	core.DashboardStore
	core.LifecycleAdmin
	core.EncryptionAdmin
	core.NotificationOutbox
}
