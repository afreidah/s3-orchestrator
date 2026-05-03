// -------------------------------------------------------------------------------
// Test Main - Counter Package Goroutine Leak Detection
//
// Author: Alex Freidah
//
// Verifies that no goroutines are leaked after all counter tests complete.
// -------------------------------------------------------------------------------

package counter

import (
	"log/slog"
	"testing"

	"go.uber.org/goleak"
)

// TestMain is the package's test entry point. Silences slog so the
// counter tests do not flood the test log with informational noise,
// and runs goleak.VerifyTestMain so any goroutine the package leaks
// (typically the per-tracker background flusher) fails the suite.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	goleak.VerifyTestMain(m)
}
