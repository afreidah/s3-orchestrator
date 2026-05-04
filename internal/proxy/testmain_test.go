// -------------------------------------------------------------------------------
// Test Main - Proxy Package Goroutine Leak Detection
//
// Author: Alex Freidah
//
// Verifies that no goroutines are leaked after all proxy tests complete.
// -------------------------------------------------------------------------------

package proxy

import (
	"log/slog"
	"testing"

	"go.uber.org/goleak"
)

// TestMain verifies the main path by exercising slog.SetDefault, slog.New, goleak.VerifyTestMain.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	goleak.VerifyTestMain(m,
		// TTLCache eviction goroutines are intentional background work
		// cleaned up by Close() in production. Many test helpers create
		// managers without t.Cleanup access.
		goleak.IgnoreTopFunction("github.com/afreidah/s3-orchestrator/internal/util/syncutil.(*TTLCache[...]).evictLoop"),
	)
}
