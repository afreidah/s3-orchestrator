// -------------------------------------------------------------------------------
// Postgres Migration Numbering Tests
//
// Author: Alex Freidah
//
// Guards the pairing between the embedded goose migrations and
// ExpectedSchemaVersion. A migration added without bumping the constant leaves
// VerifySchemaVersion rejecting every freshly migrated database; a constant
// bumped without a migration reports a version no database ever reaches.
// -------------------------------------------------------------------------------

package postgres

import (
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestMigrations_AreNumberedAndReachExpectedVersion verifies the embedded
// migrations carry distinct numeric versions and that the highest one is the
// version this binary expects.
func TestMigrations_AreNumberedAndReachExpectedVersion(t *testing.T) {
	t.Parallel()

	names, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		t.Fatalf("glob embedded migrations: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("no embedded migrations found")
	}

	versions := make([]int, 0, len(names))
	for _, name := range names {
		base := strings.TrimPrefix(name, "migrations/")
		prefix, _, ok := strings.Cut(base, "_")
		if !ok {
			t.Errorf("migration %s has no version prefix", base)
			continue
		}
		version, err := strconv.Atoi(prefix)
		if err != nil {
			t.Errorf("migration %s has a non-numeric version prefix: %v", base, err)
			continue
		}
		if version > ExpectedSchemaVersion {
			t.Errorf("migration %s is above ExpectedSchemaVersion %d", base, ExpectedSchemaVersion)
		}
		versions = append(versions, version)
	}
	if len(versions) == 0 {
		t.Fatal("no embedded migration carried a usable version prefix")
	}

	slices.Sort(versions)
	for i, version := range versions {
		if i > 0 && version == versions[i-1] {
			t.Errorf("migration version %d is used twice", version)
		}
	}
	if highest := versions[len(versions)-1]; highest != ExpectedSchemaVersion {
		t.Errorf("highest migration is %d but ExpectedSchemaVersion is %d", highest, ExpectedSchemaVersion)
	}
}
