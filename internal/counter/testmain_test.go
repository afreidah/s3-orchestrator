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

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	goleak.VerifyTestMain(m)
}
