// -------------------------------------------------------------------------------
// SQL Mutation Guard Tests
//
// Author: Alex Freidah
//
// Runs the guard over a fixture package so its failures are exercised, not
// only the passing runs the engine tests make.
// -------------------------------------------------------------------------------

package storetest

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fixtureSource holds one statement of every shape the guard distinguishes.
const fixtureSource = "package fixture\n" +
	"\n" +
	"type store struct{}\n" +
	"\n" +
	"const guardedConst = `UPDATE object_locations SET key_id = $1 WHERE storage_key = $2`\n" +
	"\n" +
	"func (s *store) Guarded() string {\n" +
	"\treturn `DELETE FROM object_locations WHERE object_key = ? AND storage_key = ?`\n" +
	"}\n" +
	"\n" +
	"func (s store) Unguarded() string {\n" +
	"\treturn `DELETE FROM object_locations WHERE object_key = ?`\n" +
	"}\n" +
	"\n" +
	"func returningOnly() string {\n" +
	"\treturn `UPDATE object_locations SET key_id = ? WHERE object_key = ? RETURNING storage_key`\n" +
	"}\n" +
	"\n" +
	"func noWhere() string {\n" +
	"\treturn `DELETE FROM object_locations`\n" +
	"}\n" +
	"\n" +
	"func exempted() string {\n" +
	"\treturn `DELETE FROM object_locations WHERE backend_name = ?`\n" +
	"}\n" +
	"\n" +
	"func unrelated() string {\n" +
	"\treturn `DELETE FROM object_locations_archive WHERE object_key = ?`\n" +
	"}\n"

// recordingTB captures the failures CheckMutations reports instead of
// failing the enclosing test.
type recordingTB struct {
	testing.TB
	errors []string
	fatal  string
}

// Helper is a no-op for the recorder.
func (r *recordingTB) Helper() {}

// Errorf records a reported failure.
func (r *recordingTB) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

// Fatalf records a fatal failure and stops the guard's goroutine, as the real
// one would.
func (r *recordingTB) Fatalf(format string, args ...any) {
	r.fatal = fmt.Sprintf(format, args...)
	panic(r)
}

// runGuard runs CheckMutations against dir with exempt, returning what it
// reported.
func runGuard(t *testing.T, dir string, exempt map[string]string) (rec *recordingTB) {
	t.Helper()
	rec = &recordingTB{TB: t}
	defer func() {
		if p := recover(); p != nil && p != rec {
			panic(p)
		}
	}()
	CheckMutations(rec, dir, CopyMutations, exempt)
	return rec
}

// writeFixture writes the fixture package into a temporary directory.
func writeFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fixture.go"), []byte(fixtureSource), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixture_test.go"), []byte("package fixture\n\nconst skipped = `DELETE FROM object_locations`\n"), 0o600); err != nil {
		t.Fatalf("write fixture test: %v", err)
	}
	return dir
}

// TestCheckMutations_ReportsUnguardedAndStaleExemptions checks that only the
// statements whose WHERE clause lacks the guard are reported, that a guard
// named only in RETURNING does not count, and that an exemption naming no
// statement is reported.
func TestCheckMutations_ReportsUnguardedAndStaleExemptions(t *testing.T) {
	rec := runGuard(t, writeFixture(t), map[string]string{
		"exempted": ExemptBackendPurge,
		"gone":     ExemptWholeKey,
	})
	var reported []string
	for _, e := range rec.errors {
		reported = append(reported, strings.SplitN(e, " ", 2)[0])
	}
	slices.Sort(reported)
	want := []string{"exemption", "noWhere", "returningOnly", "store.Unguarded"}
	if !slices.Equal(reported, want) {
		t.Fatalf("reported %v, want %v:\n%s", reported, want, strings.Join(rec.errors, "\n"))
	}
}

// TestCheckMutations_FailsOnUnreadablePackage checks that a directory with no
// Go files and a file that does not parse both stop the guard.
func TestCheckMutations_FailsOnUnreadablePackage(t *testing.T) {
	if rec := runGuard(t, t.TempDir(), nil); rec.fatal == "" {
		t.Error("empty directory did not fail the guard")
	}
	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, "broken.go"), []byte("package broken\nfunc {"), 0o600); err != nil {
		t.Fatalf("write broken file: %v", err)
	}
	if rec := runGuard(t, broken, nil); rec.fatal == "" {
		t.Error("unparseable file did not fail the guard")
	}
}
