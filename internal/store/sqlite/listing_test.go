// -------------------------------------------------------------------------------
// SQLite Store - Prefix Listing Tests
//
// Author: Alex Freidah
//
// Runs the shared prefix listing check against SQLite.
// -------------------------------------------------------------------------------

package sqlite

import (
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// TestListing runs the cross-engine prefix listing check against SQLite.
func TestListing(t *testing.T) {
	t.Parallel()
	storetest.CheckListing(t, newTestStore(t), "", "backend-a", "backend-b")
}
