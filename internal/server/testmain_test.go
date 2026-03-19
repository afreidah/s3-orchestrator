// -------------------------------------------------------------------------------
// Test Main - Server Package Test Setup
//
// Author: Alex Freidah
//
// Configures the test environment for the server package. Discards slog output
// during test runs to keep fuzz and benchmark output clean.
// -------------------------------------------------------------------------------

package server

import (
	"log/slog"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	os.Exit(m.Run())
}
