// -------------------------------------------------------------------------------
// Test Main - Drain Package Goroutine Leak Detection
//
// Author: Alex Freidah
//
// Verifies that no goroutines are leaked after all drain tests complete.
// -------------------------------------------------------------------------------

package drain

import (
	"log/slog"
	"testing"

	"go.uber.org/goleak"
)

// TestMain is the package's test entry point. Silences slog so the drain tests
// do not flood the test log, and runs goleak.VerifyTestMain so a test that
// leaves a store or backend goroutine running fails.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	goleak.VerifyTestMain(m)
}
