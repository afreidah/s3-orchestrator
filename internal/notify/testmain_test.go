// -------------------------------------------------------------------------------
// Test Main - Notify Package Goroutine Leak Detection
//
// Author: Alex Freidah
//
// Verifies that no goroutines are leaked after all notify tests complete.
// -------------------------------------------------------------------------------

package notify

import (
	"log/slog"
	"testing"

	"go.uber.org/goleak"
)

// TestMain is the package's test entry point. Silences slog so the notify
// tests do not flood the test log, and runs goleak.VerifyTestMain: the
// dispatcher owns a delivery goroutine per endpoint, so a Stop that returns
// before they exit would otherwise leak one per test.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	goleak.VerifyTestMain(m)
}
