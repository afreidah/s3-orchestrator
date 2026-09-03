// -------------------------------------------------------------------------------
// Store Test Doubles - Mock Generation Surface
//
// Author: Alex Freidah
//
// Declares the wide MetadataStore union the generated mocks are built from.
// The union exists for test doubles only: production consumers depend on the
// narrow core roles instead, which is why no such interface lives in core.
// -------------------------------------------------------------------------------

package storetest

import "github.com/afreidah/s3-orchestrator/internal/store/core"

//go:generate mockgen -destination=mocks.go -package=storetest github.com/afreidah/s3-orchestrator/internal/store/storetest MetadataStore

// Per-role mocks let a test that exercises one capability stub that
// capability alone, instead of standing up the 79-method union and
// silencing the rest with Permissive. The list carries only the roles a
// test actually mocks today - add a name when the first consumer appears
// rather than generating mocks nothing calls.
//go:generate mockgen -destination=role_mocks.go -package=storetest github.com/afreidah/s3-orchestrator/internal/store/core ObjectStore,QuotaStore,CleanupStore,ExpiredObjectsLister,BackendLifecycleStore,DashboardStore,LifecycleAdmin

// MetadataStore is the union of every narrow store role interface. It exists
// only as a mockgen target, so a single generated MockMetadataStore can stand
// in wherever a test needs a fully-populated store. Production code never
// depends on it: consumers take the narrow roles from internal/store/core,
// and the one place that holds an opened engine whole is the unexported
// composite in internal/di.
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
	core.CompressionAdmin
	core.NotificationOutbox
	core.TagStore
}
