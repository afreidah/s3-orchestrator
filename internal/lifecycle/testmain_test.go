// -------------------------------------------------------------------------------
// Test Main - Lifecycle Package Goroutine Leak Detection
//
// Author: Alex Freidah
//
// Verifies that no goroutines are leaked after all lifecycle tests complete.
// -------------------------------------------------------------------------------

package lifecycle

import (
	"log/slog"
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	goleak.VerifyTestMain(m)
}
