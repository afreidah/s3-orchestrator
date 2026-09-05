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

import "context"

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// noopTxAdapter satisfies TxAdapter with zero values throughout. Embed it,
// then override the methods a test actually exercises.
type noopTxAdapter struct{}

// Compile-time check that embedding this is enough to be a TxAdapter.
var _ TxAdapter = (*noopTxAdapter)(nil)

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

func (*noopTxAdapter) AcquireKeyLock(context.Context, string) error { return nil }

func (*noopTxAdapter) ClaimPending(context.Context, string) (bool, error) { return false, nil }

func (*noopTxAdapter) DeletePending(context.Context, string) error { return nil }

func (*noopTxAdapter) GetExistingCopiesForUpdate(context.Context, string) ([]ExistingCopy, error) {
	return nil, nil
}

func (*noopTxAdapter) InsertObjectLocation(context.Context, *ObjectLocation) error { return nil }

func (*noopTxAdapter) DeleteObjectCopies(context.Context, string) error { return nil }

func (*noopTxAdapter) GetCopiesForKeysForUpdate(context.Context, []string) ([]KeyedExistingCopy, error) {
	return nil, nil
}

func (*noopTxAdapter) DeleteObjectsByKeys(context.Context, []string) error { return nil }

func (*noopTxAdapter) CheckObjectExistsOnBackend(context.Context, string, string) (bool, error) {
	return false, nil
}

func (*noopTxAdapter) LockObjectOnBackend(context.Context, string, string) (*ObjectLocation, bool, error) {
	return nil, false, nil
}

func (*noopTxAdapter) DeleteObjectFromBackend(context.Context, string, string) error { return nil }

func (*noopTxAdapter) RecordCompressionProbe(context.Context, *CompressionProbe) error { return nil }

func (*noopTxAdapter) InsertObjectLocationIfNotExists(context.Context, *ObjectLocation) (bool, error) {
	return false, nil
}

func (*noopTxAdapter) InsertReplicaConditional(context.Context, string, string, string) (int64, bool, error) {
	return 0, false, nil
}

func (*noopTxAdapter) UpdateCompressedForm(context.Context, *CompressedUpdate) error { return nil }

func (*noopTxAdapter) MarkCopyEncrypted(context.Context, *EncryptedUpdate) error { return nil }

func (*noopTxAdapter) MarkCopyDecrypted(context.Context, string, string, int64) error { return nil }

func (*noopTxAdapter) GetCopySizeBytes(context.Context, string, string) (int64, error) {
	return 0, nil
}

func (*noopTxAdapter) SumAndDeleteCleanupQueueRows(context.Context, string, string) (int64, int64, error) {
	return 0, 0, nil
}

func (*noopTxAdapter) GetCleanupQueueRow(context.Context, int64) (CleanupQueueRow, error) {
	return CleanupQueueRow{}, nil
}

func (*noopTxAdapter) InsertCleanupDLQ(context.Context, *CleanupQueueRow) error { return nil }

func (*noopTxAdapter) DeleteCleanupItem(context.Context, int64) error { return nil }

func (*noopTxAdapter) HasPendingCleanup(context.Context, string, string) (bool, error) {
	return false, nil
}

func (*noopTxAdapter) InsertObjectTag(context.Context, string, string, string) error { return nil }

func (*noopTxAdapter) DeleteObjectTags(context.Context, string) error { return nil }

func (*noopTxAdapter) DeleteObjectTagsForKeys(context.Context, []string) error { return nil }

func (*noopTxAdapter) AdjustQuotaStripe(context.Context, string, int16, int64) error { return nil }

func (*noopTxAdapter) DecrementOrphanBytes(context.Context, string, int64) error { return nil }

func (*noopTxAdapter) AllBackendBytesUsed(context.Context) (map[string]int64, error) {
	return nil, nil
}

func (*noopTxAdapter) SumObjectSizesByBackend(context.Context) (map[string]int64, error) {
	return nil, nil
}

func (*noopTxAdapter) SetBackendBytesUsed(context.Context, string, int64) error { return nil }
