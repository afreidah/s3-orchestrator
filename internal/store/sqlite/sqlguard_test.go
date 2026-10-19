// -------------------------------------------------------------------------------
// SQLite Store - Mutation Guard Tests
//
// Author: Alex Freidah
//
// Pins every statement that changes a copy row or a cleanup row to the guard
// that keeps it from acting on a row it did not read.
// -------------------------------------------------------------------------------

package sqlite

import (
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// TestSQL_CopyMutationsMatchStorageKey checks that every UPDATE or DELETE on
// object_locations matches the copy's storage_key or is exempt for a reason.
func TestSQL_CopyMutationsMatchStorageKey(t *testing.T) {
	storetest.CheckMutations(t, ".", storetest.CopyMutations, map[string]string{
		"sqliteTxAdapter.DeleteObjectsByKeys":     storetest.ExemptWholeKey,
		"sqliteTxAdapter.DeleteObjectCopies":      storetest.ExemptWholeKey,
		"sqliteTxAdapter.DeleteObjectFromBackend": storetest.ExemptGuardedInCore,
		"Store.DeleteBackendData":                 storetest.ExemptBackendPurge,
	})
}

// TestSQL_CleanupSettlesMatchClaim checks that every UPDATE or DELETE on
// cleanup_queue matches the claim or is exempt for a reason.
func TestSQL_CleanupSettlesMatchClaim(t *testing.T) {
	storetest.CheckMutations(t, ".", storetest.CleanupSettles, map[string]string{
		"stampClaims":                      storetest.ExemptClaim,
		"Store.SweepStaleCleanupQueueRows": storetest.ExemptPathSweep,
		"Store.DeleteBackendData":          storetest.ExemptBackendPurge,
	})
}
