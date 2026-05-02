// -------------------------------------------------------------------------------
// Store Package - Type and Interface Aliases
//
// Author: Alex Freidah
//
// Re-exports the canonical role interfaces, domain types, and sentinel errors
// from internal/store/core under their original names so consumers and the
// circuit-breaker decorators in this package compile unchanged. New code
// should import from internal/store/core directly.
// -------------------------------------------------------------------------------

package store

import "github.com/afreidah/s3-orchestrator/internal/store/core"

// -------------------------------------------------------------------------
// REQUEST-TIME ROLE INTERFACES
// -------------------------------------------------------------------------

type (
	// ObjectStore aliases core.ObjectStore.
	ObjectStore = core.ObjectStore

	// QuotaStore aliases core.QuotaStore.
	QuotaStore = core.QuotaStore

	// MultipartStore aliases core.MultipartStore.
	MultipartStore = core.MultipartStore

	// ReplicationStore aliases core.ReplicationStore.
	ReplicationStore = core.ReplicationStore

	// CleanupStore aliases core.CleanupStore.
	CleanupStore = core.CleanupStore

	// PendingStore aliases core.PendingStore.
	PendingStore = core.PendingStore

	// IntegrityStore aliases core.IntegrityStore.
	IntegrityStore = core.IntegrityStore

	// ExpiredObjectsLister aliases core.ExpiredObjectsLister.
	ExpiredObjectsLister = core.ExpiredObjectsLister

	// BackendLifecycleStore aliases core.BackendLifecycleStore.
	BackendLifecycleStore = core.BackendLifecycleStore

	// UsageFlusher aliases core.UsageFlusher.
	UsageFlusher = core.UsageFlusher

	// AdvisoryLocker aliases core.AdvisoryLocker.
	AdvisoryLocker = core.AdvisoryLocker

	// DashboardStore aliases core.DashboardStore.
	DashboardStore = core.DashboardStore

	// AdminStore aliases core.AdminStore.
	AdminStore = core.AdminStore
)

// -------------------------------------------------------------------------
// DOMAIN TYPES
// -------------------------------------------------------------------------

type (
	// ObjectLocation aliases core.ObjectLocation.
	ObjectLocation = core.ObjectLocation

	// EncryptionMeta aliases core.EncryptionMeta.
	EncryptionMeta = core.EncryptionMeta

	// DeletedCopy aliases core.DeletedCopy.
	DeletedCopy = core.DeletedCopy

	// PendingObject aliases core.PendingObject.
	PendingObject = core.PendingObject

	// PendingPromoteResult aliases core.PendingPromoteResult.
	PendingPromoteResult = core.PendingPromoteResult

	// QuotaStat aliases core.QuotaStat.
	QuotaStat = core.QuotaStat

	// UsageLimits aliases core.UsageLimits.
	UsageLimits = core.UsageLimits

	// UsageStat aliases core.UsageStat.
	UsageStat = core.UsageStat

	// MultipartUpload aliases core.MultipartUpload.
	MultipartUpload = core.MultipartUpload

	// MultipartPart aliases core.MultipartPart.
	MultipartPart = core.MultipartPart

	// CleanupItem aliases core.CleanupItem.
	CleanupItem = core.CleanupItem

	// NotificationRow aliases core.NotificationRow.
	NotificationRow = core.NotificationRow

	// EncryptedLocation aliases core.EncryptedLocation.
	EncryptedLocation = core.EncryptedLocation

	// UnencryptedLocation aliases core.UnencryptedLocation.
	UnencryptedLocation = core.UnencryptedLocation

	// DecryptableLocation aliases core.DecryptableLocation.
	DecryptableLocation = core.DecryptableLocation

	// ListObjectsResult aliases core.ListObjectsResult.
	ListObjectsResult = core.ListObjectsResult

	// DirEntry aliases core.DirEntry.
	DirEntry = core.DirEntry

	// DirectoryListResult aliases core.DirectoryListResult.
	DirectoryListResult = core.DirectoryListResult

	// S3Error aliases core.S3Error.
	S3Error = core.S3Error
)

// -------------------------------------------------------------------------
// PENDING-PROMOTE RESULT ENUM
// -------------------------------------------------------------------------

const (
	// PendingPromoteCommitted aliases core.PendingPromoteCommitted.
	PendingPromoteCommitted = core.PendingPromoteCommitted

	// PendingPromoteAmbiguous aliases core.PendingPromoteAmbiguous.
	PendingPromoteAmbiguous = core.PendingPromoteAmbiguous

	// PendingPromoteAlreadyResolved aliases
	// core.PendingPromoteAlreadyResolved.
	PendingPromoteAlreadyResolved = core.PendingPromoteAlreadyResolved

	// PendingPromoteSuperseded aliases core.PendingPromoteSuperseded.
	PendingPromoteSuperseded = core.PendingPromoteSuperseded
)

// -------------------------------------------------------------------------
// SENTINEL ERRORS
// -------------------------------------------------------------------------

var (
	// ErrNoSpaceAvailable re-exports core.ErrNoSpaceAvailable.
	ErrNoSpaceAvailable = core.ErrNoSpaceAvailable

	// ErrDBUnavailable re-exports core.ErrDBUnavailable.
	ErrDBUnavailable = core.ErrDBUnavailable

	// ErrObjectNotFound re-exports core.ErrObjectNotFound.
	ErrObjectNotFound = core.ErrObjectNotFound

	// ErrMultipartUploadNotFound re-exports core.ErrMultipartUploadNotFound.
	ErrMultipartUploadNotFound = core.ErrMultipartUploadNotFound

	// ErrServiceUnavailable re-exports core.ErrServiceUnavailable.
	ErrServiceUnavailable = core.ErrServiceUnavailable

	// ErrInsufficientStorage re-exports core.ErrInsufficientStorage.
	ErrInsufficientStorage = core.ErrInsufficientStorage

	// ErrUsageLimitExceeded re-exports core.ErrUsageLimitExceeded.
	ErrUsageLimitExceeded = core.ErrUsageLimitExceeded
)

// GroupByKey re-exports core.GroupByKey for callers that still reach for it
// under the store package name.
func GroupByKey(locations []ObjectLocation) map[string][]ObjectLocation {
	return core.GroupByKey(locations)
}
