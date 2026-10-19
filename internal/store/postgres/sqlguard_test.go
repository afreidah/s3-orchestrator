// -------------------------------------------------------------------------------
// Postgres Store - Mutation Guard Tests
//
// Author: Alex Freidah
//
// Pins every sqlc statement that changes a copy row or a cleanup row to the
// guard that keeps it from acting on a row it did not read. The statements are
// the constants sqlc generates from the query files.
// -------------------------------------------------------------------------------

package postgres

import (
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// queriesDir is the sqlc-generated package holding every Postgres statement.
const queriesDir = "sqlc"

// TestSQL_CopyMutationsMatchStorageKey checks that every UPDATE or DELETE on
// object_locations matches the copy's storage_key or is exempt for a reason.
func TestSQL_CopyMutationsMatchStorageKey(t *testing.T) {
	storetest.CheckMutations(t, queriesDir, storetest.CopyMutations, map[string]string{
		"deleteObjectCopies":             storetest.ExemptWholeKey,
		"deleteObjectsByKeys":            storetest.ExemptWholeKey,
		"deleteObjectFromBackend":        storetest.ExemptGuardedInCore,
		"deleteObjectLocationsByBackend": storetest.ExemptBackendPurge,
	})
}

// TestSQL_CleanupSettlesMatchClaim checks that every UPDATE or DELETE on
// cleanup_queue matches the claim or is exempt for a reason.
func TestSQL_CleanupSettlesMatchClaim(t *testing.T) {
	storetest.CheckMutations(t, queriesDir, storetest.CleanupSettles, map[string]string{
		"claimPendingCleanups":        storetest.ExemptClaim,
		"deleteCleanupQueueByBackend": storetest.ExemptBackendPurge,
		"deleteCleanupQueueByKey":     storetest.ExemptPathSweep,
	})
}
