// -------------------------------------------------------------------------------
// No-Op TxAdapter - Shared Test Base
//
// Author: Alex Freidah
//
// Every operation in this package runs against the whole TxAdapter, while any
// one test cares about two or three of its methods. This type answers all of
// them with zero values so a stub embeds it and states only what it
// instruments - and so a method added to the adapter is implemented once here
// rather than in every stub in the package.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"time"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// noopTxAdapter satisfies TxAdapter with zero values throughout, except that
// every pending intent fits and claims, as it does for a write nothing
// overtook, and a queued cleanup row comes back carrying its request's fields.
// Embed it, then override the methods a test actually exercises.
type noopTxAdapter struct{}

// Compile-time check that embedding this is enough to be a TxAdapter.
var _ TxAdapter = (*noopTxAdapter)(nil)

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

func (*noopTxAdapter) AcquireKeyLock(context.Context, string) error { return nil }

func (*noopTxAdapter) AcquireBackendLock(context.Context, string) error { return nil }

func (*noopTxAdapter) InsertPendingIfFits(context.Context, *PendingObject, BackendFlags) (bool, error) {
	return true, nil
}

func (*noopTxAdapter) ClaimPending(context.Context, string) (bool, error) { return true, nil }

func (*noopTxAdapter) DeletePending(context.Context, string) error { return nil }

func (*noopTxAdapter) ClearPendingForKey(context.Context, string, []string) ([]SupersededIntent, error) {
	return nil, nil
}

func (*noopTxAdapter) GetExistingCopiesForUpdate(context.Context, string) ([]ExistingCopy, error) {
	return nil, nil
}

func (*noopTxAdapter) InsertObjectLocation(context.Context, *ObjectLocation) error { return nil }

func (*noopTxAdapter) DeleteObjectCopies(context.Context, string) error { return nil }

func (*noopTxAdapter) GetCopiesForKeysForUpdate(context.Context, []string) ([]KeyedExistingCopy, error) {
	return nil, nil
}

func (*noopTxAdapter) DeleteObjectsByKeys(context.Context, []string) error { return nil }

func (*noopTxAdapter) ListedPathStates(context.Context, string, []string) (map[string]PathState, error) {
	return map[string]PathState{}, nil
}

func (*noopTxAdapter) LockObjectOnBackend(context.Context, string, string) (*ObjectLocation, bool, error) {
	return nil, false, nil
}

func (*noopTxAdapter) DeleteObjectFromBackend(context.Context, string, string) error { return nil }

func (*noopTxAdapter) UpdateCopyMetadata(context.Context, *ObjectLocation, *CopyPatch) error {
	return nil
}

func (*noopTxAdapter) InsertObjectLocationIfNotExists(context.Context, *ObjectLocation) (bool, error) {
	return false, nil
}

func (*noopTxAdapter) InsertCleanup(_ context.Context, c *CleanupRequest) (CleanupItem, error) {
	return CleanupItem{
		BackendName: c.BackendName,
		ObjectKey:   c.ObjectKey,
		StorageKey:  c.StorageKey,
		Reason:      c.Reason,
		SizeBytes:   c.SizeBytes,
	}, nil
}

func (*noopTxAdapter) UpsertPart(context.Context, *RecordPartParams) (*MultipartPart, error) {
	return nil, nil
}

func (*noopTxAdapter) LockMultipartUpload(context.Context, string) (string, time.Time, error) {
	return "", time.Time{}, nil
}

func (*noopTxAdapter) DeleteMultipartUpload(context.Context, string) ([]MultipartPart, error) {
	return nil, nil
}

func (*noopTxAdapter) InsertObjectTag(context.Context, string, string, string) error { return nil }

func (*noopTxAdapter) DeleteObjectTags(context.Context, string) error { return nil }

func (*noopTxAdapter) DeleteObjectTagsForKeys(context.Context, []string) error { return nil }

func (*noopTxAdapter) AdjustQuotaStripe(context.Context, string, int16, int64) error { return nil }
