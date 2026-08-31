// -------------------------------------------------------------------------------
// Test Main - Reload Package Goroutine Leak Detection
//
// Author: Alex Freidah
//
// Verifies that no goroutines are leaked after all reload tests complete.
// -------------------------------------------------------------------------------

package reload

import (
	"log/slog"
	"testing"

	"go.uber.org/goleak"
)

// TestMain is the package's test entry point. Silences slog so the reload
// tests do not flood the test log, and runs goleak.VerifyTestMain: a reload
// hook restarts long-lived collaborators, so a hook that abandons the old one
// rather than stopping it leaks a goroutine per SIGHUP in production.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	goleak.VerifyTestMain(m)
}
