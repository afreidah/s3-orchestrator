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
//go:generate mockgen -destination=role_mocks.go -package=storetest github.com/afreidah/s3-orchestrator/internal/store/core ObjectStore,QuotaStore,CleanupStore,ExpiredObjectsLister,BackendLifecycleStore,DashboardStore,LifecycleAdmin,ProvisioningStore

// MetadataStore is the union of every narrow store role interface, used only
// as a mockgen target so one MockMetadataStore can stand in for a whole store.
// Production code depends on the narrow roles in internal/store/core instead.
// It is declared as an interface rather than built by embedding per-role mocks
// because roles share methods (GetQuotaStats), which only interface embedding
// flattens into one.
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
	core.DrainStore
	core.UsageFlusher
	core.AdvisoryLocker
	core.DashboardStore
	core.LifecycleAdmin
	core.EncryptionAdmin
	core.CompressionAdmin
	core.NotificationOutbox
	core.TagStore
	core.ProvisioningStore
}
