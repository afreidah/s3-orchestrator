// -------------------------------------------------------------------------------
// Test Main - Server Package Test Setup
//
// Author: Alex Freidah
//
// Configures the test environment for the server package. Discards slog output
// during test runs to keep fuzz and benchmark output clean.
// -------------------------------------------------------------------------------

package s3api

import (
	"log/slog"
	"testing"

	"go.uber.org/goleak"
)

// TestMain verifies the main path by exercising slog.SetDefault, slog.New, goleak.VerifyTestMain.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	goleak.VerifyTestMain(m)
}
