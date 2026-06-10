// -------------------------------------------------------------------------------
// CLI Output - Table and Key/Value Rendering Tests
//
// Author: Alex Freidah
//
// Covers the aligned table (header, dashed separator, ragged-row padding) and
// the key/value block alignment used for single-record summaries.
// -------------------------------------------------------------------------------

package output

import (
	"bytes"
	"strings"
	"testing"
)

func TestTable_HeaderSeparatorAndRows(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	err := Table(&buf, []string{"Backend", "Objects"}, [][]string{
		{"gcp", "5"},
		{"b2", "120"},
	})
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines (header, separator, 2 rows), got %d:\n%s", len(lines), buf.String())
	}
	if !strings.HasPrefix(lines[0], "Backend") || !strings.Contains(lines[0], "Objects") {
		t.Errorf("header line = %q", lines[0])
	}
	// Separator dashes are sized to each header word.
	if !strings.HasPrefix(lines[1], "-------") {
		t.Errorf("separator line = %q, want it to start with dashes sized to 'Backend'", lines[1])
	}
	if !strings.Contains(lines[2], "gcp") || !strings.Contains(lines[3], "b2") {
		t.Errorf("data rows missing: %q / %q", lines[2], lines[3])
	}
}

func TestTable_RaggedRowPadded(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	// Row shorter than the header must not panic and must still render.
	err := Table(&buf, []string{"A", "B", "C"}, [][]string{
		{"only-one"},
		{"x", "y", "z"},
	})
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	if !strings.Contains(buf.String(), "only-one") {
		t.Errorf("short row not rendered:\n%s", buf.String())
	}
}

func TestTable_EmptyRows(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := Table(&buf, []string{"Col"}, nil); err != nil {
		t.Fatalf("Table: %v", err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Errorf("expected header + separator only, got %d lines:\n%s", len(lines), buf.String())
	}
}

func TestKeyValues_Alignment(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	err := KeyValues(&buf, [][2]string{
		{"Snapshot", "snap-1"},
		{"Disk size", "100 GB"},
	})
	if err != nil {
		t.Fatalf("KeyValues: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Snapshot:") || !strings.Contains(out, "Disk size:") {
		t.Errorf("keys not rendered with colon:\n%s", out)
	}
	// Values align: the shorter key is padded to the longer key's width, so the
	// column index of each value matches.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if strings.Index(lines[0], "snap-1") != strings.Index(lines[1], "100 GB") {
		t.Errorf("values not aligned:\n%s", out)
	}
}

func TestPad(t *testing.T) {
	t.Parallel()
	if got := pad([]string{"a"}, 3); len(got) != 3 || got[0] != "a" || got[1] != "" || got[2] != "" {
		t.Errorf("pad short row = %v", got)
	}
	full := []string{"a", "b"}
	if got := pad(full, 2); len(got) != 2 {
		t.Errorf("pad exact row = %v", got)
	}
	if got := pad([]string{"a", "b", "c"}, 2); len(got) != 3 {
		t.Errorf("pad longer row should be returned as-is, got %v", got)
	}
}
