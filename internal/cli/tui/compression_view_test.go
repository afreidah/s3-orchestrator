// -------------------------------------------------------------------------------
// Compression View Tests
//
// Author: Alex Freidah
//
// Two columns exist to answer questions the rest of the panes cannot: whether a
// copy is stored as an encoding, and what that encoding is worth. Both have a
// case where the honest answer is "nothing here", and rendering that as a zero
// would read as a measurement rather than an absence.
// -------------------------------------------------------------------------------

package tui

import (
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// TestRowsFromLocations_ShowsEncoding checks the inspector distinguishes an
// encoded copy from a verbatim one, and reports the size the client wrote next
// to the size the backend holds. Without both, a compressed copy just looks
// like an object that shrank.
func TestRowsFromLocations_ShowsEncoding(t *testing.T) {
	t.Parallel()
	rows := rowsFromLocations([]adminapi.ObjectLocation{
		{Backend: "b1", SizeBytes: 250, CompressionAlgorithm: "zstd", LogicalSize: 1000},
		{Backend: "b2", SizeBytes: 1000},
	})
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}

	const logicalCol, compCol = 2, 3
	if got := rows[0][compCol]; got != "zstd" {
		t.Errorf("encoded copy COMP = %q, want %q", got, "zstd")
	}
	if got := rows[0][logicalCol]; !strings.Contains(got, "1000") {
		t.Errorf("encoded copy LOGICAL = %q, want the 1000 bytes the client wrote", got)
	}
	if got := rows[1][compCol]; got != "-" {
		t.Errorf("verbatim copy COMP = %q, want a dash", got)
	}
	if got := rows[1][logicalCol]; got != "-" {
		t.Errorf("verbatim copy LOGICAL = %q, want a dash: it is the same as SIZE", got)
	}
}

// TestOpsActions_OfferCompressionPasses checks both bulk passes are reachable
// from the ops menu. The admin API and adminctl have carried them since #1264;
// an operator driving the TUI had no way to run either.
func TestOpsActions_OfferCompressionPasses(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"/admin/api/compress-existing":   "Compress existing objects",
		"/admin/api/decompress-existing": "Decompress existing objects",
	}
	got := map[string]string{}
	for _, a := range opsActions() {
		if _, ok := want[a.path]; ok {
			got[a.path] = a.label
			if a.confirm == "" {
				t.Errorf("%s has no confirmation; it rewrites the whole fleet", a.path)
			}
		}
	}
	for path, label := range want {
		if got[path] != label {
			t.Errorf("ops menu entry for %s = %q, want %q", path, got[path], label)
		}
	}
}

// TestOpsActions_CompressionStreams checks both passes are declared as
// streaming rather than one-shot. They read and rewrite every object in the
// fleet, so a decoder here instead of a nil would leave an operator watching a
// spinner that is indistinguishable from a hung request.
func TestOpsActions_CompressionStreams(t *testing.T) {
	t.Parallel()
	streaming := map[string]bool{
		"/admin/api/compress-existing":   true,
		"/admin/api/decompress-existing": true,
	}
	for _, a := range opsActions() {
		if streaming[a.path] && a.result != nil {
			t.Errorf("%s declares a one-shot decoder; it should stream its progress", a.path)
		}
	}
}

// TestRowsFromBackends_ShowsSavings checks the backends pane reports what
// compression saved, and renders a backend saving nothing as a dash. On a fleet
// with compression off that is every row, and a column of zeroes there reads as
// a broken figure rather than an absent one.
func TestRowsFromBackends_ShowsSavings(t *testing.T) {
	t.Parallel()
	rows := rowsFromBackends([]adminapi.BackendStatus{
		{Name: "b1", CompressionSavedBytes: 750},
		{Name: "b2"},
	})
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}

	savedCol := len(rows[0]) - 1
	if got := rows[0][savedCol]; !strings.Contains(got, "750") {
		t.Errorf("SAVED = %q, want the 750 saved bytes", got)
	}
	if got := rows[1][savedCol]; got != "-" {
		t.Errorf("SAVED with nothing compressed = %q, want a dash", got)
	}
}

// TestCompressionCoverage_ReportsFleetSaving checks the backends stats line
// reports what compression is saving, and says nothing when nothing is stored
// encoded. It keys off the saving rather than the setting: a fleet that has
// just enabled compression has nothing to report yet, and one that has just
// disabled it still holds everything it compressed.
func TestCompressionCoverage_ReportsFleetSaving(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		rows []adminapi.BackendStatus
		want string
	}{
		{
			"summed across backends",
			[]adminapi.BackendStatus{
				{Name: "b1", CompressionSavedBytes: 1000},
				{Name: "b2", CompressionSavedBytes: 500},
			},
			"1.5 KiB",
		},
		{"nothing compressed", []adminapi.BackendStatus{{Name: "b1"}}, ""},
		{"no backends", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := &model{}
			m.backends.rows = tt.rows

			got := m.compressionCoverage()
			if tt.want == "" {
				if got != "" {
					t.Errorf("compressionCoverage() = %q, want nothing to report", got)
				}
				return
			}
			if !strings.Contains(got, tt.want) {
				t.Errorf("compressionCoverage() = %q, want it to contain %q", got, tt.want)
			}
		})
	}
}
