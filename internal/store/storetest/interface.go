// -------------------------------------------------------------------------------
// Store Test Doubles - Mock Generation Surface
//
// Author: Alex Freidah
//
// Names the wide MetadataStore union the generated mocks are built from.
// Production consumers depend on the narrow core roles instead.
// -------------------------------------------------------------------------------

package storetest

import "github.com/afreidah/s3-orchestrator/internal/store/engine"

//go:generate mockgen -destination=mocks.go -package=storetest github.com/afreidah/s3-orchestrator/internal/store/storetest MetadataStore

// Per-role mocks let a test that exercises one capability stub that
// capability alone, instead of standing up the whole union and
// silencing the rest with Permissive. The list carries only the roles a
// test actually mocks today - add a name when the first consumer appears
// rather than generating mocks nothing calls.
//go:generate mockgen -destination=role_mocks.go -package=storetest github.com/afreidah/s3-orchestrator/internal/store/core ObjectStore,CleanupStore,ExpiredObjectsLister,DashboardStore,LifecycleAdmin,ProvisioningStore

// MetadataStore is the mockgen target, so one MockMetadataStore can stand in
// for a whole store. It is declared here rather than built by embedding
// per-role mocks because roles share methods (GetQuotaStats), which only
// interface embedding flattens into one.
type MetadataStore interface {
	engine.Store
}
