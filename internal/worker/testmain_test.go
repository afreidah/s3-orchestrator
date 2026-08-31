// -------------------------------------------------------------------------------
// Test Main - Worker Package Goroutine Leak Detection
//
// Author: Alex Freidah
//
// Verifies that no goroutines are leaked after all worker tests complete.
// -------------------------------------------------------------------------------

package worker

import (
	"log/slog"
	"testing"

	"go.uber.org/goleak"
)

// TestMain is the package's test entry point. Silences slog so the worker
// tests do not flood the test log, and runs goleak.VerifyTestMain: this
// package owns the tick loops and bounded worker pools, so a pass that stops
// reporting but leaves its goroutines running is exactly what would otherwise
// go unnoticed.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	goleak.VerifyTestMain(m)
}
