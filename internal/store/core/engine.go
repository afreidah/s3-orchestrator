// -------------------------------------------------------------------------------
// Engine Role Set - Compile-Time Check for a Store Implementation
//
// Author: Alex Freidah
//
// One statement of "an engine implements every role", used by each engine to
// assert itself. Stated as a generic constraint rather than an interface,
// because the set is a fact about there being one store type per engine and
// not a contract any consumer may hold: a constraint cannot be a variable
// type, so nothing can take the whole store by depending on it.
// -------------------------------------------------------------------------------

package core

// engineRoles is every role a metadata-store engine implements. Unexported so
// it cannot be named outside this package; engines reach it through
// AssertEngine.
type engineRoles interface {
	Runner
	ObjectStore
	QuotaStore
	MultipartStore
	ReplicationStore
	CleanupStore
	PendingStore
	IntegrityStore
	ExpiredObjectsLister
	BackendLifecycleStore
	UsageFlusher
	AdvisoryLocker
	DashboardStore
	LifecycleAdmin
	EncryptionAdmin
	CompressionAdmin
	NotificationOutbox
	TagStore
}

// AssertEngine fails to compile unless T implements every store role, naming
// the missing method. Each engine declares one blank var against it:
//
//	var _ = core.AssertEngine[*Store]
func AssertEngine[T engineRoles]() {
	// Intentionally empty: instantiating it is the check, so there is
	// nothing to run and nothing ever calls it.
}
