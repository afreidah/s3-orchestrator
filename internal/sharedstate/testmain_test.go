// -------------------------------------------------------------------------------
// Test Main - Shared State Package Goroutine Leak Detection
//
// Author: Alex Freidah
//
// Verifies that no watcher or heartbeat goroutine outlives the tests.
// -------------------------------------------------------------------------------

package sharedstate

import (
	"log/slog"
	"testing"

	"go.uber.org/goleak"
)

// TestMain silences slog and fails the suite on a leaked goroutine, which here
// would be a Watch or Run that did not return when its context ended.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	goleak.VerifyTestMain(m)
}
